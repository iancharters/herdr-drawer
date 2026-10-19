package main

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	frameData   byte = 0x01
	frameResize byte = 0x02
	maxRingSize      = 128 * 1024
)

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func getWinsize(fd uintptr) (uint16, uint16) {
	var ws winsize
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); errno != 0 || ws.Row == 0 || ws.Col == 0 {
		return 24, 80
	}
	return ws.Row, ws.Col
}

func setWinsize(fd uintptr, rows, cols uint16) {
	if rows == 0 || cols == 0 {
		return
	}
	ws := winsize{Row: rows, Col: cols}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (r *ringBuffer) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > maxRingSize {
		r.buf = append([]byte(nil), r.buf[len(r.buf)-maxRingSize:]...)
	}
}

func (r *ringBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}

type sessionHandshake struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	Rows      uint16 `json:"rows,omitempty"`
	Cols      uint16 `json:"cols,omitempty"`
}

func daemonSocketPath() string {
	return filepath.Join(stateDir(), "daemon.sock")
}

func daemonLockPath() string {
	return filepath.Join(stateDir(), "daemon.lock")
}

func newSessionID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "s-" + hex.EncodeToString(b[:])
}

func callDaemon(hs sessionHandshake, out interface{}) error {
	conn, err := net.DialTimeout("unix", daemonSocketPath(), 300*time.Millisecond)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(1500 * time.Millisecond))

	reqBytes, err := json.Marshal(hs)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(reqBytes, '\n')); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(line, out)
	}
	return nil
}

func isDaemonAlive() bool {
	var resp struct {
		Alive bool `json:"alive"`
	}
	if err := callDaemon(sessionHandshake{Type: "daemon_ping"}, &resp); err != nil {
		return false
	}
	return resp.Alive
}

func ensureDaemon() error {
	if isDaemonAlive() {
		return nil
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(daemonLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	if isDaemonAlive() {
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()

	cmd := exec.Command(exe, "daemon")
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if isDaemonAlive() {
			return nil
		}
		time.Sleep(15 * time.Millisecond)
	}
	return errors.New("timed out waiting for herdr-drawer daemon")
}

func isSessionAlive(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	var resp struct {
		Alive bool `json:"alive"`
	}
	if err := callDaemon(sessionHandshake{Type: "ping", SessionID: sessionID}, &resp); err != nil {
		return false
	}
	return resp.Alive
}

func killSession(sessionID string) {
	if sessionID == "" {
		return
	}
	_ = callDaemon(sessionHandshake{Type: "kill", SessionID: sessionID}, nil)
}

func sessionCwd(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	var resp struct {
		Cwd string `json:"cwd"`
	}
	if err := callDaemon(sessionHandshake{Type: "cwd", SessionID: sessionID}, &resp); err != nil {
		return ""
	}
	return resp.Cwd
}

func listDaemonSessions() []string {
	var resp struct {
		Sessions []string `json:"sessions"`
	}
	if err := callDaemon(sessionHandshake{Type: "list"}, &resp); err != nil {
		return nil
	}
	return resp.Sessions
}

func ensureSessionHost(sessionID, cwd string, rows, cols uint16) error {
	if sessionID == "" {
		return errors.New("missing session ID")
	}
	if err := ensureDaemon(); err != nil {
		return err
	}
	var resp struct {
		Alive bool   `json:"alive"`
		Error string `json:"error,omitempty"`
	}
	if err := callDaemon(sessionHandshake{
		Type:      "ensure",
		SessionID: sessionID,
		Cwd:       cwd,
		Rows:      rows,
		Cols:      cols,
	}, &resp); err != nil {
		return err
	}
	if !resp.Alive {
		if resp.Error != "" {
			return errors.New(resp.Error)
		}
		return fmt.Errorf("failed to start session %s", sessionID)
	}
	return nil
}

func resolveProcessCwd(pid int, fallback string) string {
	if pid <= 0 {
		return fallback
	}
	if link, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil && link != "" {
		return link
	}
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "n/") {
				return strings.TrimSpace(line[1:])
			}
		}
	}
	return fallback
}

type ptySession struct {
	id         string
	initialCwd string
	master     *os.File
	cmd        *exec.Cmd
	ring       ringBuffer

	clientMu   sync.Mutex
	activeConn net.Conn
	doneCh     chan struct{}
	termOnce   sync.Once
}

func (s *ptySession) isAttached() bool {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	return s.activeConn != nil
}

func (s *ptySession) setActiveConn(c net.Conn) {
	s.clientMu.Lock()
	old := s.activeConn
	s.activeConn = c
	s.clientMu.Unlock()
	if old != nil && old != c {
		_ = old.Close()
	}
}

func (s *ptySession) terminate() {
	s.termOnce.Do(func() {
		if s.cmd != nil && s.cmd.Process != nil {
			pgid := s.cmd.Process.Pid
			_ = syscall.Kill(-pgid, syscall.SIGHUP)
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			go func() {
				time.Sleep(300 * time.Millisecond)
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}()
		}
		s.setActiveConn(nil)
		if s.master != nil {
			_ = s.master.Close()
		}
	})
}

func (s *ptySession) currentCwd() string {
	pid := 0
	if s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}
	return resolveProcessCwd(pid, s.initialCwd)
}

type daemonServer struct {
	mu           sync.Mutex
	sessions     map[string]*ptySession
	ln           net.Listener
	doneCh       chan struct{}
	disconnectCh chan struct{}
	stopOnce     sync.Once
}

func newDaemonServer(ln net.Listener) *daemonServer {
	return &daemonServer{
		sessions:     make(map[string]*ptySession),
		ln:           ln,
		doneCh:       make(chan struct{}),
		disconnectCh: make(chan struct{}, 1),
	}
}

func (d *daemonServer) notifyDisconnect() {
	select {
	case d.disconnectCh <- struct{}{}:
	default:
	}
}

func (d *daemonServer) shutdown() {
	d.stopOnce.Do(func() {
		close(d.doneCh)
		if d.ln != nil {
			_ = d.ln.Close()
		}
		d.mu.Lock()
		list := make([]*ptySession, 0, len(d.sessions))
		for _, s := range d.sessions {
			list = append(list, s)
		}
		d.sessions = make(map[string]*ptySession)
		d.mu.Unlock()
		for _, s := range list {
			s.terminate()
		}
	})
}

func (d *daemonServer) getSession(sessionID string) *ptySession {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessions[sessionID]
}

func (d *daemonServer) getOrCreateSession(sessionID, cwd string, initRows, initCols uint16) (*ptySession, error) {
	if sessionID == "" {
		return nil, errors.New("missing session ID")
	}
	d.mu.Lock()
	if existing, ok := d.sessions[sessionID]; ok {
		select {
		case <-existing.doneCh:
			delete(d.sessions, sessionID)
		default:
			d.mu.Unlock()
			return existing, nil
		}
	}
	d.mu.Unlock()

	if initRows == 0 {
		initRows = 24
	}
	if initCols == 0 {
		initCols = 80
	}

	master, slave, err := openPTY()
	if err != nil {
		return nil, err
	}
	setWinsize(master.Fd(), initRows, initCols)

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	} else if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		cwd, _ = os.UserHomeDir()
	}

	cmd := exec.Command(shell, "-l")
	cmd.Dir = cwd
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}

	var env []string
	hasTerm := false
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HERDR_DRAWER_SESSION_ID=") ||
			strings.HasPrefix(kv, "HERDR_DRAWER_CWD=") ||
			strings.HasPrefix(kv, "HERDR_PANE_ID=") ||
			strings.HasPrefix(kv, "HERDR_PLUGIN_ACTION_ID=") ||
			strings.HasPrefix(kv, "HERDR_PLUGIN_ENTRYPOINT_ID=") ||
			strings.HasPrefix(kv, "HERDR_PLUGIN_CONTEXT_JSON=") {
			continue
		}
		if strings.HasPrefix(kv, "TERM=") {
			hasTerm = true
		}
		env = append(env, kv)
	}
	if !hasTerm {
		env = append(env, "TERM=xterm-256color")
	}
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return nil, err
	}
	_ = slave.Close()

	sess := &ptySession{
		id:         sessionID,
		initialCwd: cwd,
		master:     master,
		cmd:        cmd,
		doneCh:     make(chan struct{}),
	}

	d.mu.Lock()
	if raced, ok := d.sessions[sessionID]; ok {
		d.mu.Unlock()
		sess.terminate()
		return raced, nil
	}
	d.sessions[sessionID] = sess
	d.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		close(sess.doneCh)
		d.mu.Lock()
		if d.sessions[sessionID] == sess {
			delete(d.sessions, sessionID)
		}
		d.mu.Unlock()
		sess.setActiveConn(nil)
		_ = master.Close()
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				sess.ring.Write(chunk)
				sess.clientMu.Lock()
				c := sess.activeConn
				sess.clientMu.Unlock()
				if c != nil {
					if _, werr := c.Write(chunk); werr != nil {
						sess.clientMu.Lock()
						if sess.activeConn == c {
							sess.activeConn = nil
						}
						sess.clientMu.Unlock()
						_ = c.Close()
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	return sess, nil
}

func (d *daemonServer) handleConn(c net.Conn) {
	reader := bufio.NewReader(c)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		_ = c.Close()
		return
	}
	var hs sessionHandshake
	if err := json.Unmarshal(line, &hs); err != nil {
		_ = c.Close()
		return
	}

	switch hs.Type {
	case "daemon_ping":
		_, _ = c.Write([]byte(`{"alive":true}` + "\n"))
		_ = c.Close()

	case "ensure":
		_, err := d.getOrCreateSession(hs.SessionID, hs.Cwd, hs.Rows, hs.Cols)
		if err != nil {
			resp, _ := json.Marshal(map[string]any{"alive": false, "error": err.Error()})
			_, _ = c.Write(append(resp, '\n'))
			_ = c.Close()
			return
		}
		_, _ = c.Write([]byte(`{"alive":true}` + "\n"))
		_ = c.Close()

	case "ping":
		sess := d.getSession(hs.SessionID)
		alive := false
		if sess != nil {
			select {
			case <-sess.doneCh:
			default:
				alive = true
			}
		}
		resp, _ := json.Marshal(map[string]bool{"alive": alive})
		_, _ = c.Write(append(resp, '\n'))
		_ = c.Close()

	case "cwd":
		sess := d.getSession(hs.SessionID)
		cwd := ""
		if sess != nil {
			cwd = sess.currentCwd()
		}
		resp, _ := json.Marshal(map[string]string{"cwd": cwd})
		_, _ = c.Write(append(resp, '\n'))
		_ = c.Close()

	case "kill":
		d.mu.Lock()
		sess := d.sessions[hs.SessionID]
		delete(d.sessions, hs.SessionID)
		d.mu.Unlock()
		if sess != nil {
			sess.terminate()
		}
		_, _ = c.Write([]byte(`{"ok":true}` + "\n"))
		_ = c.Close()

	case "list":
		d.mu.Lock()
		ids := make([]string, 0, len(d.sessions))
		for id := range d.sessions {
			ids = append(ids, id)
		}
		d.mu.Unlock()
		resp, _ := json.Marshal(map[string][]string{"sessions": ids})
		_, _ = c.Write(append(resp, '\n'))
		_ = c.Close()

	case "attach":
		sess, err := d.getOrCreateSession(hs.SessionID, hs.Cwd, hs.Rows, hs.Cols)
		if err != nil {
			_ = c.Close()
			return
		}
		if hs.Rows > 0 && hs.Cols > 0 {
			setWinsize(sess.master.Fd(), hs.Rows, hs.Cols)
		}
		sess.clientMu.Lock()
		old := sess.activeConn
		sess.activeConn = c
		snap := sess.ring.Snapshot()
		if len(snap) > 0 {
			_, _ = c.Write(snap)
		}
		sess.clientMu.Unlock()
		if old != nil && old != c {
			_ = old.Close()
		}

		var hdr [5]byte
		for {
			if _, err := io.ReadFull(reader, hdr[:1]); err != nil {
				break
			}
			switch hdr[0] {
			case frameData:
				if _, err := io.ReadFull(reader, hdr[1:3]); err != nil {
					goto done
				}
				length := int(binary.BigEndian.Uint16(hdr[1:3]))
				if length == 0 {
					continue
				}
				payload := make([]byte, length)
				if _, err := io.ReadFull(reader, payload); err != nil {
					goto done
				}
				if _, err := sess.master.Write(payload); err != nil {
					goto done
				}
			case frameResize:
				if _, err := io.ReadFull(reader, hdr[1:5]); err != nil {
					goto done
				}
				r := binary.BigEndian.Uint16(hdr[1:3])
				cols := binary.BigEndian.Uint16(hdr[3:5])
				setWinsize(sess.master.Fd(), r, cols)
			default:
				goto done
			}
		}
	done:
		wasActive := false
		sess.clientMu.Lock()
		if sess.activeConn == c {
			sess.activeConn = nil
			wasActive = true
		}
		sess.clientMu.Unlock()
		_ = c.Close()
		if wasActive {
			d.notifyDisconnect()
		}

	default:
		_ = c.Close()
	}
}

func (d *daemonServer) reconcileSessions() {
	sockPath, err := resolveSocketPath()
	if err != nil {
		d.shutdown()
		return
	}
	client := &RPCClient{socketPath: sockPath}
	panes, err := client.ListPanes()
	if err != nil {
		d.shutdown()
		return
	}
	sf, _ := readStateSnapshot()

	d.mu.Lock()
	snapshot := make([]*ptySession, 0, len(d.sessions))
	for _, s := range d.sessions {
		snapshot = append(snapshot, s)
	}
	d.mu.Unlock()

	for _, sess := range snapshot {
		if !shouldKeepSessionAliveWithSnapshot(sess.id, sess.isAttached(), panes, sf) {
			d.mu.Lock()
			if d.sessions[sess.id] == sess {
				delete(d.sessions, sess.id)
			}
			d.mu.Unlock()
			sess.terminate()
		}
	}
}

type herdrEventEnvelope struct {
	Event string `json:"event"`
	Data  struct {
		Pane PaneInfo `json:"pane"`
	} `json:"data"`
}

func (d *daemonServer) watchHerdrEvents() {
	sockPath, err := resolveSocketPath()
	if err != nil {
		return
	}

	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		d.shutdown()
		return
	}
	defer conn.Close()

	go func() {
		<-d.doneCh
		_ = conn.Close()
	}()

	req := rpcRequest{
		ID:     "daemon-watch",
		Method: "events.subscribe",
		Params: map[string]any{
			"subscriptions": []map[string]any{
				{"type": "pane.created"},
				{"type": "pane.closed"},
				{"type": "tab.closed"},
				{"type": "workspace.closed"},
			},
		},
	}
	reqBytes, _ := json.Marshal(req)
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(append(reqBytes, '\n')); err != nil {
		d.shutdown()
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})

	type streamMsg struct {
		line []byte
		err  error
	}
	msgCh := make(chan streamMsg, 32)
	go func() {
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadBytes('\n')
			msgCh <- streamMsg{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()

	var verifyCh <-chan time.Time

	for {
		select {
		case <-d.doneCh:
			return
		case <-d.disconnectCh:
			verifyCh = time.After(350 * time.Millisecond)
		case <-verifyCh:
			verifyCh = nil
			d.reconcileSessions()
		case msg := <-msgCh:
			if msg.err != nil {
				d.shutdown()
				return
			}
			var ev herdrEventEnvelope
			if err := json.Unmarshal(msg.line, &ev); err != nil || ev.Event == "" {
				continue
			}
			switch ev.Event {
			case "pane_created":
				if ev.Data.Pane.Label == "" && ev.Data.Pane.TabID != "" {
					_ = adoptNativeDrawerSplits(ev.Data.Pane.TabID)
				}
			case "pane_closed", "tab_closed", "workspace_closed":
				verifyCh = time.After(350 * time.Millisecond)
			}
		}
	}
}

func runDaemon() error {
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	sock := daemonSocketPath()
	if isDaemonAlive() {
		return nil
	}
	_ = os.Remove(sock)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	_ = os.Chmod(sock, 0o600)
	defer func() {
		_ = ln.Close()
		_ = os.Remove(sock)
	}()

	srv := newDaemonServer(ln)
	defer srv.shutdown()

	if os.Getenv("HERDR_DRAWER_NO_WATCHDOG") == "" {
		go srv.watchHerdrEvents()
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil
		}
		go srv.handleConn(conn)
	}
}

func shouldKeepSessionAliveWithSnapshot(sessionID string, attached bool, panes []PaneInfo, sf *StateFile) bool {
	if attached {
		return true
	}
	if sf == nil {
		return false
	}

	activeTabs := make(map[string]bool, len(panes))
	termToTab := make(map[string]string, len(panes))
	for _, p := range panes {
		activeTabs[p.TabID] = true
		termToTab[p.TerminalID] = p.TabID
	}

	for tabID, ts := range sf.Tabs {
		sessions := map[string]bool{}
		collectDrawerSessions(ts.Tree, sessions)
		for _, s := range ts.TermToSession {
			if s != "" {
				sessions[s] = true
			}
		}
		if !sessions[sessionID] {
			continue
		}
		if !activeTabs[tabID] {
			return false
		}
		if ts.AnchorTerminalID != "" && termToTab[ts.AnchorTerminalID] != tabID {
			return false
		}
		if ts.Visible {
			for termID, s := range ts.TermToSession {
				if s == sessionID && termToTab[termID] == tabID {
					return true
				}
			}
			return false
		}
		return true
	}

	return false
}

func runAttach(sessionID, cwd string) error {
	if sessionID == "" {
		sessionID = os.Getenv("HERDR_DRAWER_SESSION_ID")
	}
	if cwd == "" {
		cwd = os.Getenv("HERDR_DRAWER_CWD")
	}
	if sessionID == "" {
		return errors.New("HERDR_DRAWER_SESSION_ID is not set")
	}

	rows, cols := getWinsize(os.Stdin.Fd())
	if err := ensureDaemon(); err != nil {
		return err
	}

	conn, err := net.DialTimeout("unix", daemonSocketPath(), 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	origTermios, _ := makeRaw(os.Stdin.Fd())
	defer restoreTermios(os.Stdin.Fd(), origTermios)

	hsBytes, _ := json.Marshal(sessionHandshake{
		Type:      "attach",
		SessionID: sessionID,
		Cwd:       cwd,
		Rows:      rows,
		Cols:      cols,
	})
	if _, err := conn.Write(append(hsBytes, '\n')); err != nil {
		return err
	}

	var writeMu sync.Mutex
	sendResize := func() {
		r, c := getWinsize(os.Stdin.Fd())
		var pkt [5]byte
		pkt[0] = frameResize
		binary.BigEndian.PutUint16(pkt[1:3], r)
		binary.BigEndian.PutUint16(pkt[3:5], c)
		writeMu.Lock()
		_, _ = conn.Write(pkt[:])
		writeMu.Unlock()
	}

	winchCh := make(chan os.Signal, 4)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(winchCh)

	termCh := make(chan os.Signal, 2)
	signal.Notify(termCh, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(termCh)

	doneCh := make(chan struct{})
	go func() {
		for {
			select {
			case <-winchCh:
				sendResize()
			case <-termCh:
				_ = conn.Close()
				return
			case <-doneCh:
				return
			}
		}
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				pkt := make([]byte, 3+n)
				pkt[0] = frameData
				binary.BigEndian.PutUint16(pkt[1:3], uint16(n))
				copy(pkt[3:], buf[:n])
				writeMu.Lock()
				_, werr := conn.Write(pkt)
				writeMu.Unlock()
				if werr != nil {
					_ = conn.Close()
					return
				}
			}
			if err != nil {
				_ = conn.Close()
				return
			}
		}
	}()

	_, _ = io.Copy(os.Stdout, conn)
	close(doneCh)
	return nil
}

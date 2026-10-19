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
	Type string `json:"type"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

func sessionsDir() string {
	return filepath.Join(stateDir(), "sessions")
}

func sessionSocketPath(sessionID string) string {
	return filepath.Join(sessionsDir(), sessionID+".sock")
}

func newSessionID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "s-" + hex.EncodeToString(b[:])
}

func isSessionAlive(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	sock := sessionSocketPath(sessionID)
	conn, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := conn.Write([]byte(`{"type":"ping"}` + "\n")); err != nil {
		return false
	}
	var resp struct {
		Alive bool `json:"alive"`
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return false
	}
	_ = json.Unmarshal(line, &resp)
	return resp.Alive
}

func killSession(sessionID string) {
	if sessionID == "" {
		return
	}
	sock := sessionSocketPath(sessionID)
	conn, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if err != nil {
		_ = os.Remove(sock)
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
	_, _ = conn.Write([]byte(`{"type":"kill"}` + "\n"))
}

func sessionCwd(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sock := sessionSocketPath(sessionID)
	conn, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
	if err != nil {
		return ""
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(400 * time.Millisecond))
	if _, err := conn.Write([]byte(`{"type":"cwd"}` + "\n")); err != nil {
		return ""
	}
	var resp struct {
		Cwd string `json:"cwd"`
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return ""
	}
	_ = json.Unmarshal(line, &resp)
	return resp.Cwd
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

func ensureSessionHost(sessionID, cwd string, rows, cols uint16) error {
	if isSessionAlive(sessionID) {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(sessionsDir(), 0o700); err != nil {
		return err
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()

	cmd := exec.Command(
		exe,
		"host",
		"--session", sessionID,
		"--cwd", cwd,
		"--rows", strconv.Itoa(int(rows)),
		"--cols", strconv.Itoa(int(cols)),
	)
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
		if isSessionAlive(sessionID) {
			return nil
		}
		time.Sleep(15 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for session host %s", sessionID)
}

func runHost(sessionID, cwd string, initRows, initCols uint16) error {
	if sessionID == "" {
		return errors.New("missing session ID")
	}
	if err := os.MkdirAll(sessionsDir(), 0o700); err != nil {
		return err
	}
	sock := sessionSocketPath(sessionID)
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

	master, slave, err := openPTY()
	if err != nil {
		return err
	}
	defer master.Close()
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
		return err
	}
	_ = slave.Close()

	var (
		clientMu   sync.Mutex
		activeConn net.Conn
		ring       ringBuffer
	)

	setActiveConn := func(c net.Conn) {
		clientMu.Lock()
		old := activeConn
		activeConn = c
		clientMu.Unlock()
		if old != nil && old != c {
			_ = old.Close()
		}
	}

	terminateShell := func() {
		if cmd.Process == nil {
			return
		}
		pgid := cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGHUP)
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}()
		_ = ln.Close()
	}

	disconnectCh := make(chan struct{}, 1)
	notifyDisconnect := func() {
		select {
		case disconnectCh <- struct{}{}:
		default:
		}
	}

	doneCh := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(doneCh)
		_ = ln.Close()
		_ = os.Remove(sock)
		setActiveConn(nil)
		_ = master.Close()
	}()

	if os.Getenv("HERDR_DRAWER_NO_WATCHDOG") == "" {
		isAttached := func() bool {
			clientMu.Lock()
			defer clientMu.Unlock()
			return activeConn != nil
		}
		go watchHerdrEvents(sessionID, doneCh, disconnectCh, isAttached, terminateShell)
	}

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				ring.Write(chunk)
				clientMu.Lock()
				c := activeConn
				clientMu.Unlock()
				if c != nil {
					if _, werr := c.Write(chunk); werr != nil {
						clientMu.Lock()
						if activeConn == c {
							activeConn = nil
						}
						clientMu.Unlock()
						_ = c.Close()
					}
				}
			}
			if err != nil {
				_ = ln.Close()
				return
			}
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil
		}
		go func(c net.Conn) {
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
			case "ping":
				_, _ = c.Write([]byte(`{"alive":true}` + "\n"))
				_ = c.Close()
				return
			case "cwd":
				pid := 0
				if cmd.Process != nil {
					pid = cmd.Process.Pid
				}
				respBytes, _ := json.Marshal(map[string]string{
					"cwd": resolveProcessCwd(pid, cwd),
				})
				_, _ = c.Write(append(respBytes, '\n'))
				_ = c.Close()
				return
			case "kill":
				_, _ = c.Write([]byte(`{"ok":true}` + "\n"))
				_ = c.Close()
				terminateShell()
				return
			case "attach":
				if hs.Rows > 0 && hs.Cols > 0 {
					setWinsize(master.Fd(), hs.Rows, hs.Cols)
				}
				clientMu.Lock()
				old := activeConn
				activeConn = c
				snap := ring.Snapshot()
				if len(snap) > 0 {
					_, _ = c.Write(snap)
				}
				clientMu.Unlock()
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
						if _, err := master.Write(payload); err != nil {
							goto done
						}
					case frameResize:
						if _, err := io.ReadFull(reader, hdr[1:5]); err != nil {
							goto done
						}
						r := binary.BigEndian.Uint16(hdr[1:3])
						cols := binary.BigEndian.Uint16(hdr[3:5])
						setWinsize(master.Fd(), r, cols)
					default:
						goto done
					}
				}
			done:
				wasActive := false
				clientMu.Lock()
				if activeConn == c {
					activeConn = nil
					wasActive = true
				}
				clientMu.Unlock()
				_ = c.Close()
				if wasActive {
					notifyDisconnect()
				}
			default:
				_ = c.Close()
			}
		}(conn)
	}
}

type herdrEventEnvelope struct {
	Event string `json:"event"`
	Data  struct {
		Pane PaneInfo `json:"pane"`
	} `json:"data"`
}

func watchHerdrEvents(sessionID string, doneCh <-chan struct{}, disconnectCh <-chan struct{}, isAttached func() bool, terminateShell func()) {
	sockPath, err := resolveSocketPath()
	if err != nil {
		return
	}

	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		terminateShell()
		return
	}
	defer conn.Close()

	go func() {
		<-doneCh
		_ = conn.Close()
	}()

	req := rpcRequest{
		ID:     "host-watch",
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
		terminateShell()
		return
	}
	_ = conn.SetWriteDeadline(time.Time{})

	type streamMsg struct {
		line []byte
		err  error
	}
	msgCh := make(chan streamMsg, 16)
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

	verifyCh := time.After(2 * time.Second)

	for {
		select {
		case <-doneCh:
			return
		case <-disconnectCh:
			verifyCh = time.After(350 * time.Millisecond)
		case <-verifyCh:
			verifyCh = nil
			if !shouldKeepSessionAlive(sessionID, isAttached()) {
				terminateShell()
				return
			}
		case msg := <-msgCh:
			if msg.err != nil {
				terminateShell()
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

func shouldKeepSessionAlive(sessionID string, attached bool) bool {
	sockPath, err := resolveSocketPath()
	if err != nil {
		return false
	}
	client := &RPCClient{socketPath: sockPath}
	panes, err := client.ListPanes()
	if err != nil {
		return false
	}
	if attached {
		return true
	}

	sf, err := readStateSnapshot()
	if err != nil || sf == nil {
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
	if err := ensureSessionHost(sessionID, cwd, rows, cols); err != nil {
		return err
	}

	conn, err := net.DialTimeout("unix", sessionSocketPath(sessionID), 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	origTermios, _ := makeRaw(os.Stdin.Fd())
	defer restoreTermios(os.Stdin.Fd(), origTermios)

	hsBytes, _ := json.Marshal(sessionHandshake{
		Type: "attach",
		Rows: rows,
		Cols: cols,
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

package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFindDrawerSplitAndSnapshot(t *testing.T) {
	root := &LayoutNode{
		Type:      "split",
		Direction: "right",
		Ratio:     0.75,
		First: &LayoutNode{
			Type:      "split",
			Direction: "down",
			Ratio:     0.70,
			First: &LayoutNode{
				Type:   "pane",
				PaneID: "w4:p1",
			},
			Second: &LayoutNode{
				Type:      "split",
				Direction: "right",
				Ratio:     0.50,
				First: &LayoutNode{
					Type:   "pane",
					PaneID: "w4:p5",
				},
				Second: &LayoutNode{
					Type:   "pane",
					PaneID: "w4:p6",
				},
			},
		},
		Second: &LayoutNode{
			Type:   "pane",
			PaneID: "w4:p4",
		},
	}

	paneByID := map[string]PaneInfo{
		"w4:p1": {PaneID: "w4:p1", TerminalID: "term_nvim", Cwd: "/repo"},
		"w4:p4": {PaneID: "w4:p4", TerminalID: "term_agent", Cwd: "/repo"},
		"w4:p5": {PaneID: "w4:p5", TerminalID: "term_d1", Cwd: "/repo/sub1"},
		"w4:p6": {PaneID: "w4:p6", TerminalID: "term_d2", Cwd: "/repo/sub2"},
	}
	paneToTerm := map[string]string{
		"w4:p1": "term_nvim",
		"w4:p4": "term_agent",
		"w4:p5": "term_d1",
		"w4:p6": "term_d2",
	}
	termToSession := map[string]string{
		"term_d1": "s-111",
		"term_d2": "s-222",
	}

	split := findDrawerSplit(root, "w4:p1", paneToTerm, termToSession, "down", false)
	if split == nil {
		t.Fatalf("expected to find drawer split")
	}
	if split.Ratio != 0.70 {
		t.Fatalf("expected drawer split ratio 0.70, got %v", split.Ratio)
	}

	tree := layoutToDrawerTree(split.Second, paneByID, termToSession)
	if tree == nil || tree.Type != "split" || tree.Direction != "right" || tree.Ratio != 0.50 {
		t.Fatalf("unexpected drawer tree: %+v", tree)
	}
	leaves := collectDrawerLeavesOrdered(tree)
	expectedLeaves := []string{"s-111", "s-222"}
	if !reflect.DeepEqual(leaves, expectedLeaves) {
		t.Fatalf("expected leaves %v, got %v", expectedLeaves, leaves)
	}

	path, ok := findSplitPath(root, "w4:p5", "w4:p6", nil)
	if !ok {
		t.Fatalf("expected to find split path for w4:p5 and w4:p6")
	}
	expectedPath := []bool{false, true}
	if !reflect.DeepEqual(path, expectedPath) {
		t.Fatalf("expected path %v, got %v", expectedPath, path)
	}

	sib := findSiblingSessionID(split.Second, "w4:p6", paneToTerm, termToSession)
	if sib != "s-111" {
		t.Fatalf("expected sibling session s-111 for w4:p6, got %q", sib)
	}
}

func TestFindDrawerSplitLeftPlacement(t *testing.T) {
	root := &LayoutNode{
		Type:      "split",
		Direction: "right",
		Ratio:     0.3,
		First: &LayoutNode{
			Type:   "pane",
			PaneID: "w1:pU",
		},
		Second: &LayoutNode{
			Type:   "pane",
			PaneID: "w1:pA",
		},
	}
	paneToTerm := map[string]string{
		"w1:pU": "term_u",
		"w1:pA": "term_a",
	}
	termToSession := map[string]string{
		"term_u": "s-utility",
	}
	split := findDrawerSplit(root, "w1:pA", paneToTerm, termToSession, "left", false)
	if split == nil {
		t.Fatalf("expected left-placement split to be found")
	}
	utility, anchor := splitBranch(split, normalizePlacement("left").utilityOnFirst)
	if !layoutContainsPane(utility, "w1:pU") || !layoutContainsPane(anchor, "w1:pA") {
		t.Fatalf("unexpected left split branches")
	}
}

func TestSplitRatioForPlacement(t *testing.T) {
	if got := splitRatioForPlacement(0.30, "down"); got != 0.70 {
		t.Fatalf("down ratio mismatch: got %v", got)
	}
	if got := splitRatioForPlacement(0.30, "left"); got != 0.30 {
		t.Fatalf("left ratio mismatch: got %v", got)
	}
}

func TestPruneDrawerTree(t *testing.T) {
	tree := &DrawerNode{
		Type:      "split",
		Direction: "right",
		Ratio:     0.4,
		First: &DrawerNode{
			Type:      "split",
			Direction: "down",
			Ratio:     0.6,
			First:     &DrawerNode{Type: "leaf", SessionID: "s-1", Cwd: "/a"},
			Second:    &DrawerNode{Type: "leaf", SessionID: "s-2", Cwd: "/b"},
		},
		Second: &DrawerNode{Type: "leaf", SessionID: "s-3", Cwd: "/c"},
	}

	alive := map[string]bool{
		"s-2": true,
		"s-3": true,
	}

	pruned := pruneDrawerTree(tree, func(sessID string) bool {
		return alive[sessID]
	})
	if pruned == nil || pruned.Type != "split" || pruned.Direction != "right" || pruned.Ratio != 0.4 {
		t.Fatalf("expected top-level right split to survive, got %+v", pruned)
	}
	if pruned.First == nil || pruned.First.Type != "leaf" || pruned.First.SessionID != "s-2" {
		t.Fatalf("expected first child to collapse to s-2 leaf, got %+v", pruned.First)
	}
	if pruned.Second == nil || pruned.Second.Type != "leaf" || pruned.Second.SessionID != "s-3" {
		t.Fatalf("expected second child to be s-3 leaf, got %+v", pruned.Second)
	}
}

func TestRingBuffer(t *testing.T) {
	var r ringBuffer
	chunk := make([]byte, maxRingSize+10)
	for i := range chunk {
		chunk[i] = byte(i % 251)
	}
	r.Write(chunk)
	snap := r.Snapshot()
	if len(snap) != maxRingSize {
		t.Fatalf("expected ring buffer length %d, got %d", maxRingSize, len(snap))
	}
	if !reflect.DeepEqual(snap, chunk[10:]) {
		t.Fatalf("ring buffer contents mismatch after overflow")
	}
}

func TestSanitizeShellEnv(t *testing.T) {
	t.Setenv("TERM", "dumb")
	t.Setenv("NO_COLOR", "1")
	t.Setenv("FORCE_COLOR", "0")
	t.Setenv("CURSOR_AGENT", "1")
	env := sanitizeShellEnv("dumb")
	hasXterm := false
	for _, kv := range env {
		if kv == "TERM=xterm-256color" {
			hasXterm = true
		}
		if strings.HasPrefix(kv, "NO_COLOR=") || strings.HasPrefix(kv, "FORCE_COLOR=") || strings.HasPrefix(kv, "CURSOR_") || kv == "TERM=dumb" {
			t.Fatalf("unexpected unsanitized env var: %s", kv)
		}
	}
	if !hasXterm {
		t.Fatalf("expected TERM=xterm-256color in sanitized env")
	}
}

func TestSessionHostPersistAcrossReconnect(t *testing.T) {
	testDir := "/tmp/herdr-drawer-test-" + newSessionID()
	defer os.RemoveAll(testDir)
	t.Setenv("HERDR_DRAWER_STATE_DIR", testDir)
	t.Setenv("HERDR_DRAWER_NO_WATCHDOG", "1")
	sessID := newSessionID()
	sessID2 := newSessionID()
	defer killSession(sessID)
	defer killSession(sessID2)

	go func() {
		_ = runDaemon()
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !isDaemonAlive() {
		time.Sleep(15 * time.Millisecond)
	}
	if !isDaemonAlive() {
		t.Fatalf("daemon did not become ready")
	}

	if err := ensureSessionHost(sessID, "/tmp", 24, 80); err != nil {
		t.Fatalf("ensureSessionHost 1: %v", err)
	}
	if err := ensureSessionHost(sessID2, "/tmp", 24, 80); err != nil {
		t.Fatalf("ensureSessionHost 2: %v", err)
	}
	if !isSessionAlive(sessID) || !isSessionAlive(sessID2) {
		t.Fatalf("sessions did not become ready")
	}
	if got := len(listDaemonSessions()); got != 2 {
		t.Fatalf("expected 2 daemon sessions, got %d", got)
	}

	conn1, err := net.DialTimeout("unix", daemonSocketPath(), time.Second)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	_, _ = conn1.Write([]byte(`{"type":"attach","session_id":"` + sessID + `","rows":24,"cols":80}` + "\n"))

	cmdStr := "export DRAWER_PERSIST_VAR=alive_42\n"
	pkt := make([]byte, 3+len(cmdStr))
	pkt[0] = frameData
	binary.BigEndian.PutUint16(pkt[1:3], uint16(len(cmdStr)))
	copy(pkt[3:], cmdStr)
	_, _ = conn1.Write(pkt)
	time.Sleep(150 * time.Millisecond)
	_ = conn1.Close()

	conn2, err := net.DialTimeout("unix", daemonSocketPath(), time.Second)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer conn2.Close()
	_, _ = conn2.Write([]byte(`{"type":"attach","session_id":"` + sessID + `","rows":24,"cols":80}` + "\n"))

	cmdStr2 := "echo RESULT_${DRAWER_PERSIST_VAR}_END\n"
	pkt2 := make([]byte, 3+len(cmdStr2))
	pkt2[0] = frameData
	binary.BigEndian.PutUint16(pkt2[1:3], uint16(len(cmdStr2)))
	copy(pkt2[3:], cmdStr2)
	_, _ = conn2.Write(pkt2)

	_ = conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16384)
	var collected strings.Builder
	for !strings.Contains(collected.String(), "RESULT_alive_42_END") {
		n, err := conn2.Read(buf)
		if n > 0 {
			collected.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	if !strings.Contains(collected.String(), "RESULT_alive_42_END") {
		t.Fatalf("expected persisted env var across reconnect, got output: %q", collected.String())
	}
}

func TestRunSetupInteractiveDefaultAndOverride(t *testing.T) {
	tmpConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpConfig)

	var out bytes.Buffer
	if err := runSetup("", true, strings.NewReader("\n\n"), &out); err != nil {
		t.Fatalf("runSetup default: %v", err)
	}
	expectedDefault := filepath.Join(tmpConfig, "nvim", "lua", "plugins", "herdr-drawer.lua")
	data, err := os.ReadFile(expectedDefault)
	if err != nil {
		t.Fatalf("read default config: %v", err)
	}
	if !strings.Contains(string(data), "editor/nvim.lua") || !strings.Contains(string(data), "return {}") {
		t.Fatalf("unexpected loader content: %s", string(data))
	}
	if !strings.Contains(out.String(), "Restart Neovim for the changes to take effect.") {
		t.Fatalf("expected restart prompt in output: %s", out.String())
	}

	customPath := filepath.Join(tmpConfig, "custom", "my-drawer.lua")
	out.Reset()
	if err := runSetup("", true, strings.NewReader(customPath+"\n\n"), &out); err != nil {
		t.Fatalf("runSetup override: %v", err)
	}
	if _, err := os.Stat(customPath); err != nil {
		t.Fatalf("expected custom config at %s: %v", customPath, err)
	}
}

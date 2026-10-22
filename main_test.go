package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockHerdrServer struct {
	mu          sync.Mutex
	sockPath    string
	ln          net.Listener
	nextPaneNum int
	nextTabNum  int
	panes       map[string]*PaneInfo
	paneOrder   []string
	tabs        map[string]*LayoutNode
	processes   map[string][]ForegroundProcess
	sentText           map[string][]string
	popupCalls         int
	agentHolders       map[string]string
	agentReportEvents  []string
	agentSessionEvents []string
	explainState       string
	explainReason      string
	explainSkipUpdate  bool
}

func newMockHerdrServer(t *testing.T) *mockHerdrServer {
	t.Helper()
	sockPath := filepath.Join("/tmp", fmt.Sprintf("hd-mock-%s.sock", newSessionID()))
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen mock herdr: %v", err)
	}
	s := &mockHerdrServer{
		sockPath:     sockPath,
		ln:           ln,
		nextPaneNum:  1,
		nextTabNum:   1,
		panes:        make(map[string]*PaneInfo),
		tabs:         make(map[string]*LayoutNode),
		processes:    make(map[string][]ForegroundProcess),
		sentText:     make(map[string][]string),
		agentHolders: make(map[string]string),
		explainState: "idle",
	}
	go s.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.Remove(sockPath)
	})
	return s
}

func (s *mockHerdrServer) addInitialPane(workspaceID, tabID, terminalID, cwd string, focused bool) *PaneInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	paneID := fmt.Sprintf("%s:p%d", workspaceID, s.nextPaneNum)
	s.nextPaneNum++
	p := &PaneInfo{
		PaneID:        paneID,
		TabID:         tabID,
		WorkspaceID:   workspaceID,
		TerminalID:    terminalID,
		Cwd:           cwd,
		ForegroundCwd: cwd,
		Focused:       focused,
	}
	s.panes[paneID] = p
	s.paneOrder = append(s.paneOrder, paneID)
	if s.tabs[tabID] == nil {
		s.tabs[tabID] = &LayoutNode{Type: "pane", PaneID: paneID}
	} else {
		s.tabs[tabID] = &LayoutNode{
			Type:      "split",
			Direction: "right",
			Ratio:     0.5,
			First:     s.tabs[tabID],
			Second:    &LayoutNode{Type: "pane", PaneID: paneID},
		}
	}
	return p
}

func (s *mockHerdrServer) setProcesses(paneID string, procs []ForegroundProcess) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processes[paneID] = procs
}

func (s *mockHerdrServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func replacePaneWithSplit(root *LayoutNode, targetPaneID string, splitNode *LayoutNode) (*LayoutNode, bool) {
	if root == nil {
		return nil, false
	}
	if root.Type == "pane" {
		if root.PaneID == targetPaneID {
			return splitNode, true
		}
		return root, false
	}
	if next, ok := replacePaneWithSplit(root.First, targetPaneID, splitNode); ok {
		root.First = next
		return root, true
	}
	if next, ok := replacePaneWithSplit(root.Second, targetPaneID, splitNode); ok {
		root.Second = next
		return root, true
	}
	return root, false
}

func removePaneFromLayout(root *LayoutNode, targetPaneID string) (*LayoutNode, bool) {
	if root == nil {
		return nil, false
	}
	if root.Type == "pane" {
		if root.PaneID == targetPaneID {
			return nil, true
		}
		return root, false
	}
	if root.First != nil && root.First.Type == "pane" && root.First.PaneID == targetPaneID {
		return root.Second, true
	}
	if root.Second != nil && root.Second.Type == "pane" && root.Second.PaneID == targetPaneID {
		return root.First, true
	}
	if next, ok := removePaneFromLayout(root.First, targetPaneID); ok {
		if next == nil {
			return root.Second, true
		}
		root.First = next
		return root, true
	}
	if next, ok := removePaneFromLayout(root.Second, targetPaneID); ok {
		if next == nil {
			return root.First, true
		}
		root.Second = next
		return root, true
	}
	return root, false
}

func swapPanesInLayout(root *LayoutNode, paneA, paneB string) {
	if root == nil {
		return
	}
	if root.Type == "pane" {
		switch root.PaneID {
		case paneA:
			root.PaneID = paneB
		case paneB:
			root.PaneID = paneA
		}
		return
	}
	swapPanesInLayout(root.First, paneA, paneB)
	swapPanesInLayout(root.Second, paneA, paneB)
}

func (s *mockHerdrServer) focusPaneLocked(paneID string) {
	target, ok := s.panes[paneID]
	if !ok {
		return
	}
	for _, p := range s.panes {
		if p.TabID == target.TabID {
			p.Focused = (p.PaneID == paneID)
		}
	}
}

func (s *mockHerdrServer) handleConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		ID     string          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}

	if req.Method == "events.subscribe" {
		// Keep connection open until server closes.
		buf := make([]byte, 64)
		_, _ = conn.Read(buf)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	replyOK := func(res any) {
		raw, _ := json.Marshal(res)
		env := map[string]any{
			"id":     req.ID,
			"result": json.RawMessage(raw),
		}
		b, _ := json.Marshal(env)
		_, _ = conn.Write(append(b, '\n'))
	}
	replyErr := func(code, msg string) {
		env := map[string]any{
			"id": req.ID,
			"error": map[string]string{
				"code":    code,
				"message": msg,
			},
		}
		b, _ := json.Marshal(env)
		_, _ = conn.Write(append(b, '\n'))
	}

	switch req.Method {
	case "pane.list":
		out := make([]PaneInfo, 0, len(s.paneOrder))
		for _, id := range s.paneOrder {
			if p, ok := s.panes[id]; ok {
				out = append(out, *p)
			}
		}
		replyOK(map[string]any{"panes": out})

	case "pane.get":
		var params struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(req.Params, &params)
		p, ok := s.panes[params.PaneID]
		if !ok {
			replyErr("not_found", "pane not found")
			return
		}
		replyOK(map[string]any{"pane": *p})

	case "pane.process_info":
		var params struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(req.Params, &params)
		procs := s.processes[params.PaneID]
		replyOK(map[string]any{
			"process_info": ProcessInfo{
				PaneID:              params.PaneID,
				ForegroundProcesses: procs,
			},
		})

	case "layout.export":
		var params struct {
			TabID string `json:"tab_id"`
		}
		_ = json.Unmarshal(req.Params, &params)
		root := s.tabs[params.TabID]
		if root == nil {
			replyErr("not_found", "tab not found")
			return
		}
		focusedID := ""
		wsID := "w1"
		for _, id := range s.paneOrder {
			if p, ok := s.panes[id]; ok && p.TabID == params.TabID {
				wsID = p.WorkspaceID
				if p.Focused {
					focusedID = p.PaneID
				}
			}
		}
		replyOK(map[string]any{
			"layout": LayoutExport{
				WorkspaceID:   wsID,
				TabID:         params.TabID,
				FocusedPaneID: focusedID,
				Root:          *cloneLayoutNode(root),
			},
		})

	case "plugin.pane.open":
		var params struct {
			Placement    string `json:"placement"`
			Direction    string `json:"direction"`
			TargetPaneID string `json:"target_pane_id"`
			Cwd          string `json:"cwd"`
			Focus        bool   `json:"focus"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if params.Placement == "popup" {
			s.popupCalls++
			replyOK(map[string]any{})
			return
		}
		target, ok := s.panes[params.TargetPaneID]
		if !ok {
			replyErr("not_found", "target pane not found")
			return
		}
		paneID := fmt.Sprintf("%s:p%d", target.WorkspaceID, s.nextPaneNum)
		termID := fmt.Sprintf("term_%d", s.nextPaneNum)
		s.nextPaneNum++
		cwd := params.Cwd
		if cwd == "" {
			cwd = target.Cwd
		}
		newPane := &PaneInfo{
			PaneID:        paneID,
			TabID:         target.TabID,
			WorkspaceID:   target.WorkspaceID,
			TerminalID:    termID,
			Label:         "Herdr Drawer",
			Cwd:           cwd,
			ForegroundCwd: cwd,
			Focused:       params.Focus,
		}
		s.panes[paneID] = newPane
		s.paneOrder = append(s.paneOrder, paneID)
		if params.Focus {
			s.focusPaneLocked(paneID)
		}
		splitNode := &LayoutNode{
			Type:      "split",
			Direction: params.Direction,
			Ratio:     0.5,
			First:     &LayoutNode{Type: "pane", PaneID: target.PaneID},
			Second:    &LayoutNode{Type: "pane", PaneID: paneID},
		}
		if root := s.tabs[target.TabID]; root != nil {
			if updated, replaced := replacePaneWithSplit(root, target.PaneID, splitNode); replaced {
				s.tabs[target.TabID] = updated
			}
		}
		replyOK(map[string]any{
			"plugin_pane": map[string]any{
				"pane": *newPane,
			},
		})

	case "pane.swap":
		var params struct {
			SourcePaneID string `json:"source_pane_id"`
			TargetPaneID string `json:"target_pane_id"`
		}
		_ = json.Unmarshal(req.Params, &params)
		for _, root := range s.tabs {
			swapPanesInLayout(root, params.SourcePaneID, params.TargetPaneID)
		}
		replyOK(map[string]any{})

	case "layout.set_split_ratio":
		var params struct {
			TabID string  `json:"tab_id"`
			Path  []bool  `json:"path"`
			Ratio float64 `json:"ratio"`
		}
		_ = json.Unmarshal(req.Params, &params)
		cur := s.tabs[params.TabID]
		for _, step := range params.Path {
			if cur == nil || cur.Type != "split" {
				break
			}
			if !step {
				cur = cur.First
			} else {
				cur = cur.Second
			}
		}
		if cur != nil && cur.Type == "split" {
			cur.Ratio = params.Ratio
		}
		replyOK(map[string]any{})

	case "pane.move":
		var params struct {
			PaneID      string `json:"pane_id"`
			Focus       bool   `json:"focus"`
			Destination struct {
				Type         string  `json:"type"`
				WorkspaceID  string  `json:"workspace_id"`
				TabID        string  `json:"tab_id"`
				Split        string  `json:"split"`
				TargetPaneID string  `json:"target_pane_id"`
				Ratio        float64 `json:"ratio"`
			} `json:"destination"`
		}
		_ = json.Unmarshal(req.Params, &params)
		pane, ok := s.panes[params.PaneID]
		if !ok {
			replyErr("not_found", "pane not found")
			return
		}
		oldTabID := pane.TabID
		if oldRoot := s.tabs[oldTabID]; oldRoot != nil {
			updated, _ := removePaneFromLayout(oldRoot, pane.PaneID)
			if updated == nil {
				delete(s.tabs, oldTabID)
			} else {
				s.tabs[oldTabID] = updated
			}
		}

		if params.Destination.Type == "new_tab" {
			s.nextTabNum++
			newTabID := fmt.Sprintf("%s:t_stage_%d", pane.WorkspaceID, s.nextTabNum)
			pane.TabID = newTabID
			s.tabs[newTabID] = &LayoutNode{Type: "pane", PaneID: pane.PaneID}
			replyOK(map[string]any{
				"move_result": map[string]any{
					"created_tab": map[string]any{
						"tab_id": newTabID,
					},
				},
			})
			return
		}

		destTabID := params.Destination.TabID
		pane.TabID = destTabID
		ratio := params.Destination.Ratio
		if ratio < 0.1 || ratio > 0.9 {
			ratio = 0.5
		}
		dir := params.Destination.Split
		if dir == "" {
			dir = "right"
		}
		destRoot := s.tabs[destTabID]
		if destRoot == nil {
			s.tabs[destTabID] = &LayoutNode{Type: "pane", PaneID: pane.PaneID}
		} else if params.Destination.TargetPaneID != "" {
			splitNode := &LayoutNode{
				Type:      "split",
				Direction: dir,
				Ratio:     ratio,
				First:     &LayoutNode{Type: "pane", PaneID: params.Destination.TargetPaneID},
				Second:    &LayoutNode{Type: "pane", PaneID: pane.PaneID},
			}
			if updated, replaced := replacePaneWithSplit(destRoot, params.Destination.TargetPaneID, splitNode); replaced {
				s.tabs[destTabID] = updated
			}
		} else {
			s.tabs[destTabID] = &LayoutNode{
				Type:      "split",
				Direction: dir,
				Ratio:     ratio,
				First:     destRoot,
				Second:    &LayoutNode{Type: "pane", PaneID: pane.PaneID},
			}
		}
		if params.Focus {
			s.focusPaneLocked(pane.PaneID)
		}
		replyOK(map[string]any{})

	case "pane.close":
		var params struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if pane, ok := s.panes[params.PaneID]; ok {
			delete(s.panes, params.PaneID)
			if root := s.tabs[pane.TabID]; root != nil {
				updated, _ := removePaneFromLayout(root, params.PaneID)
				if updated == nil {
					delete(s.tabs, pane.TabID)
				} else {
					s.tabs[pane.TabID] = updated
				}
			}
		}
		replyOK(map[string]any{})

	case "pane.focus":
		var params struct {
			PaneID string `json:"pane_id"`
		}
		_ = json.Unmarshal(req.Params, &params)
		s.focusPaneLocked(params.PaneID)
		replyOK(map[string]any{})

	case "pane.send_text":
		var params struct {
			PaneID string `json:"pane_id"`
			Text   string `json:"text"`
		}
		_ = json.Unmarshal(req.Params, &params)
		s.sentText[params.PaneID] = append(s.sentText[params.PaneID], params.Text)
		replyOK(map[string]any{})

	case "pane.report_agent":
		var params struct {
			PaneID  string `json:"pane_id"`
			Agent   string `json:"agent"`
			State   string `json:"state"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if params.State == "unknown" {
			delete(s.agentHolders, params.PaneID)
		} else {
			s.agentHolders[params.PaneID] = params.Agent
		}
		s.agentReportEvents = append(s.agentReportEvents, fmt.Sprintf("%s:%s:%s:%s", params.PaneID, params.Agent, params.State, params.Message))
		replyOK(map[string]any{})

	case "pane.report_agent_session":
		var params struct {
			PaneID     string   `json:"pane_id"`
			Agent      string   `json:"agent"`
			SessionID  string   `json:"agent_session_id"`
			ResumeArgv []string `json:"resume_argv"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if len(params.ResumeArgv) > 0 && s.agentHolders[params.PaneID] != params.Agent {
			replyErr("resume_not_accepted", "resume_argv requires the reporter to hold the pane; report its state with pane.report_agent first")
			return
		}
		s.agentSessionEvents = append(s.agentSessionEvents, fmt.Sprintf("%s:%s:%s:%s", params.PaneID, params.Agent, params.SessionID, strings.Join(params.ResumeArgv, " ")))
		replyOK(map[string]any{})

	case "agent.explain":
		var params struct {
			Target string `json:"target"`
		}
		_ = json.Unmarshal(req.Params, &params)
		replyOK(map[string]any{
			"pane": map[string]any{
				"pane_id":      params.Target,
				"agent_kind":   s.agentHolders[params.Target],
				"agent_status": s.explainState,
			},
			"explain": map[string]any{
				"state":             s.explainState,
				"reason":            s.explainReason,
				"skip_state_update": s.explainSkipUpdate,
			},
		})

	default:
		replyOK(map[string]any{})
	}
}

func startTestEnv(t *testing.T, profilesTOML string) (*mockHerdrServer, string) {
	t.Helper()
	srv := newMockHerdrServer(t)
	stateDirPath := filepath.Join("/tmp", "hd-state-"+newSessionID())
	cfgDirPath := t.TempDir()
	if err := os.MkdirAll(stateDirPath, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	if profilesTOML != "" {
		if err := os.WriteFile(filepath.Join(cfgDirPath, "profiles.toml"), []byte(profilesTOML), 0o644); err != nil {
			t.Fatalf("write profiles.toml: %v", err)
		}
	}
	t.Setenv("HERDR_SOCKET_PATH", srv.sockPath)
	t.Setenv("HERDR_DRAWER_STATE_DIR", stateDirPath)
	t.Setenv("HERDR_DRAWER_CONFIG_DIR", cfgDirPath)
	t.Setenv("HERDR_DRAWER_NO_WATCHDOG", "1")
	t.Setenv("HERDR_PANE_ID", "")

	go func() {
		_ = runDaemon()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !isDaemonAlive() {
		time.Sleep(15 * time.Millisecond)
	}
	if !isDaemonAlive() {
		t.Fatalf("daemon did not start")
	}
	t.Cleanup(func() {
		shutdownDaemon()
		_ = os.RemoveAll(stateDirPath)
	})
	return srv, stateDirPath
}

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

	split := findDrawerSplit(root, "w4:p1", paneToTerm, termToSession, "down", "anchor", false)
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
	split := findDrawerSplit(root, "w1:pA", paneToTerm, termToSession, "left", "anchor", false)
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
	if got := splitRatioForPlacement(0.25, "up"); got != 0.25 {
		t.Fatalf("up ratio mismatch: got %v", got)
	}
	if got := splitRatioForPlacement(0.25, "right"); got != 0.75 {
		t.Fatalf("right ratio mismatch: got %v", got)
	}
	if got := splitRatioForPlacement(0.99, "down"); got != 0.70 {
		t.Fatalf("out-of-bounds ratio should clamp to 0.30 utility -> 0.70 split ratio, got %v", got)
	}
}

func TestFindDrawerSplitTabScope(t *testing.T) {
	root := &LayoutNode{
		Type:      "split",
		Direction: "down",
		Ratio:     0.7,
		First: &LayoutNode{
			Type:      "split",
			Direction: "right",
			Ratio:     0.5,
			First:     &LayoutNode{Type: "pane", PaneID: "w1:p1"},
			Second:    &LayoutNode{Type: "pane", PaneID: "w1:p2"},
		},
		Second: &LayoutNode{Type: "pane", PaneID: "w1:pU"},
	}
	paneToTerm := map[string]string{
		"w1:p1": "term1",
		"w1:p2": "term2",
		"w1:pU": "termU",
	}
	termToSession := map[string]string{
		"termU": "s-u",
	}
	split := findDrawerSplit(root, "w1:p1", paneToTerm, termToSession, "down", "tab", false)
	if split == nil || split != root {
		t.Fatalf("expected tab-scope split to resolve to root")
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

func TestRingBufferPreservesEvictedTerminalModes(t *testing.T) {
	var r ringBuffer
	r.Write([]byte("\x1b[>1u\x1b[?2004h\x1b[?1004h\x1b[?25l"))

	filler := bytes.Repeat([]byte("A"), maxRingSize+512)
	r.Write(filler)

	snap := r.Snapshot()
	wantPrefix := "\x1b[?25l\x1b[?1004h\x1b[?2004h\x1b[>1u"
	if !strings.HasPrefix(string(snap), wantPrefix) {
		t.Fatalf("expected snapshot to start with restored modes %q, got prefix %q", wantPrefix, string(snap[:min(len(snap), 64)]))
	}

	// If the application later exits and disables those modes, once evicted they must no longer be emitted.
	r.Write([]byte("\x1b[<u\x1b[?2004l\x1b[?1004l\x1b[?25h"))
	r.Write(filler)

	snapAfterExit := r.Snapshot()
	if strings.Contains(string(snapAfterExit), "\x1b[>1u") || strings.Contains(string(snapAfterExit), "\x1b[?2004h") {
		t.Fatalf("expected disabled modes to be cleared after eviction, got prefix %q", string(snapAfterExit[:min(len(snapAfterExit), 64)]))
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

func TestShouldKeepSessionAliveWithProfileStateKeys(t *testing.T) {
	sf := &StateFile{
		Tabs: map[string]*TabDrawerState{
			stateKey("w1:t1", "default"): {
				ProfileID:        "default",
				TabID:            "w1:t1",
				WorkspaceID:      "w1",
				AnchorTerminalID: "term_anchor",
				Visible:          false,
				TermToSession:    map[string]string{},
				Tree: &DrawerNode{
					Type:      "split",
					Direction: "right",
					Ratio:     0.5,
					First:     &DrawerNode{Type: "leaf", SessionID: "s-hidden-1", Cwd: "/tmp"},
					Second:    &DrawerNode{Type: "leaf", SessionID: "s-hidden-2", Cwd: "/tmp"},
				},
			},
			stateKey("w1:t1", "cursorRight"): {
				ProfileID:        "cursorRight",
				TabID:            "w1:t1",
				WorkspaceID:      "w1",
				AnchorTerminalID: "term_anchor",
				Visible:          true,
				TermToSession: map[string]string{
					"term_cursor": "s-visible-alive",
					"term_closed": "s-visible-closed",
				},
				Tree: &DrawerNode{
					Type:      "leaf",
					SessionID: "s-visible-alive",
					Cwd:       "/tmp",
				},
			},
		},
	}

	panes := []PaneInfo{
		{PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1", TerminalID: "term_anchor"},
		{PaneID: "w1:p2", TabID: "w1:t1", WorkspaceID: "w1", TerminalID: "term_cursor"},
	}

	if !shouldKeepSessionAliveWithSnapshot("s-hidden-1", false, panes, sf) {
		t.Fatalf("expected hidden drawer session s-hidden-1 to be kept alive when tab is active")
	}
	if !shouldKeepSessionAliveWithSnapshot("s-hidden-2", false, panes, sf) {
		t.Fatalf("expected hidden split drawer session s-hidden-2 to be kept alive when tab is active")
	}
	if !shouldKeepSessionAliveWithSnapshot("s-visible-alive", false, panes, sf) {
		t.Fatalf("expected visible session with active terminal to be kept alive")
	}
	if shouldKeepSessionAliveWithSnapshot("s-visible-closed", false, panes, sf) {
		t.Fatalf("expected visible session whose terminal was closed to be reaped")
	}
	if shouldKeepSessionAliveWithSnapshot("s-hidden-1", false, nil, sf) {
		t.Fatalf("expected session in closed tab to be reaped")
	}
}

func TestHiddenStartupCommandDoesNotEchoOrFeedStdin(t *testing.T) {
	testDir := "/tmp/herdr-drawer-cmdtest-" + newSessionID()
	defer os.RemoveAll(testDir)
	t.Setenv("HERDR_DRAWER_STATE_DIR", testDir)
	t.Setenv("HERDR_DRAWER_NO_WATCHDOG", "1")
	sessID := newSessionID()
	defer killSession(sessID)

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

	pyCmd := `python3 -c "import sys, select; sys.stdout.write('BANNER_READY\n'); sys.stdout.flush(); r, _, _ = select.select([sys.stdin], [], [], 0.25); data = sys.stdin.readline().strip() if r else 'EMPTY'; sys.stdout.write('STDIN_WAS:' + data + '_END\n'); sys.stdout.flush()"`
	if err := ensureSessionHostWithCommand(sessID, "/tmp", 24, 80, pyCmd, false); err != nil {
		t.Fatalf("ensureSessionHostWithCommand: %v", err)
	}

	conn, err := net.DialTimeout("unix", daemonSocketPath(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(`{"type":"attach","session_id":"` + sessID + `","rows":24,"cols":80}` + "\n"))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16384)
	var collected strings.Builder
	for !strings.Contains(collected.String(), "_END") {
		n, err := conn.Read(buf)
		if n > 0 {
			collected.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	out := collected.String()
	if !strings.Contains(out, "BANNER_READY") {
		t.Fatalf("expected BANNER_READY in output, got %q", out)
	}
	if !strings.Contains(out, "STDIN_WAS:EMPTY_END") {
		t.Fatalf("expected empty stdin (no prefilled command), got %q", out)
	}
	if strings.Contains(out, "python3 -c") {
		t.Fatalf("expected startup command not to be echoed in output when showCommand=false, got %q", out)
	}
}

func TestInteractiveStartupCommandEchoesAndRuns(t *testing.T) {
	testDir := "/tmp/herdr-drawer-showcmd-" + newSessionID()
	defer os.RemoveAll(testDir)
	t.Setenv("HERDR_DRAWER_STATE_DIR", testDir)
	t.Setenv("HERDR_DRAWER_NO_WATCHDOG", "1")
	sessID := newSessionID()
	defer killSession(sessID)

	go func() {
		_ = runDaemon()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !isDaemonAlive() {
		time.Sleep(15 * time.Millisecond)
	}

	if err := ensureSessionHostWithCommand(sessID, "/tmp", 24, 80, "echo SHOW_CMD_MARKER_99", true); err != nil {
		t.Fatalf("ensureSessionHostWithCommand(show=true): %v", err)
	}
	conn, err := net.DialTimeout("unix", daemonSocketPath(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(`{"type":"attach","session_id":"` + sessID + `","rows":24,"cols":80}` + "\n"))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16384)
	var collected strings.Builder
	for !strings.Contains(collected.String(), "SHOW_CMD_MARKER_99") {
		n, err := conn.Read(buf)
		if n > 0 {
			collected.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	if !strings.Contains(collected.String(), "SHOW_CMD_MARKER_99") {
		t.Fatalf("expected interactive command output, got %q", collected.String())
	}
}

func TestFindDrawerSplitIgnoresSameDirectionInnerSplit(t *testing.T) {
	innerSplit := &LayoutNode{
		Type:      "split",
		Direction: "down",
		Ratio:     0.5,
		First:     &LayoutNode{Type: "pane", PaneID: "w1:p2"},
		Second:    &LayoutNode{Type: "pane", PaneID: "w1:p3"},
	}
	outerSplit := &LayoutNode{
		Type:      "split",
		Direction: "down",
		Ratio:     0.7,
		First:     &LayoutNode{Type: "pane", PaneID: "w1:p1"},
		Second:    innerSplit,
	}
	paneToTerm := map[string]string{
		"w1:p1": "term_anchor",
		"w1:p2": "term_u1",
		"w1:p3": "term_u2",
	}
	termToSession := map[string]string{
		"term_u1": "s-1",
		"term_u2": "s-2",
	}
	got := findDrawerSplit(outerSplit, "w1:p1", paneToTerm, termToSession, "down", "anchor", false)
	if got != outerSplit {
		t.Fatalf("expected outerSplit, got %+v", got)
	}
}

func TestStageAndReconstructLayoutSubtreeBothBranches(t *testing.T) {
	srv := newMockHerdrServer(t)
	client := &RPCClient{socketPath: srv.sockPath}

	p1 := srv.addInitialPane("w1", "w1:t1", "term1", "/tmp", true)
	p2 := srv.addInitialPane("w1", "w1:t1", "term2", "/tmp", false)
	p3 := srv.addInitialPane("w1", "w1:t1", "term3", "/tmp", false)

	// Case 1: keptPaneID in First branch (p1).
	layout1, err := client.ExportLayout("w1:t1")
	if err != nil {
		t.Fatalf("export layout 1: %v", err)
	}
	saved1, staging1, kept1, err := stageLayoutSubtree(client, "w1", &layout1.Root, p1.PaneID)
	if err != nil {
		t.Fatalf("stageLayoutSubtree 1: %v", err)
	}
	if staging1 == "" || kept1 != p1.PaneID {
		t.Fatalf("unexpected stage result: staging=%q kept=%q", staging1, kept1)
	}
	if err := reconstructLayoutSubtree(client, "w1:t1", saved1, kept1); err != nil {
		t.Fatalf("reconstructLayoutSubtree 1: %v", err)
	}
	after1, err := client.ExportLayout("w1:t1")
	if err != nil {
		t.Fatalf("export after 1: %v", err)
	}
	if !reflect.DeepEqual(collectLayoutPaneIDs(&after1.Root), []string{p1.PaneID, p2.PaneID, p3.PaneID}) {
		t.Fatalf("expected pane order [%s %s %s], got %v", p1.PaneID, p2.PaneID, p3.PaneID, collectLayoutPaneIDs(&after1.Root))
	}

	// Case 2: keptPaneID in Second branch (p3).
	saved2, staging2, kept2, err := stageLayoutSubtree(client, "w1", &after1.Root, p3.PaneID)
	if err != nil {
		t.Fatalf("stageLayoutSubtree 2: %v", err)
	}
	if staging2 == "" || kept2 != p3.PaneID {
		t.Fatalf("unexpected stage 2 result: staging=%q kept=%q", staging2, kept2)
	}
	if err := reconstructLayoutSubtree(client, "w1:t1", saved2, kept2); err != nil {
		t.Fatalf("reconstructLayoutSubtree 2: %v", err)
	}
	after2, err := client.ExportLayout("w1:t1")
	if err != nil {
		t.Fatalf("export after 2: %v", err)
	}
	if !reflect.DeepEqual(collectLayoutPaneIDs(&after2.Root), []string{p1.PaneID, p2.PaneID, p3.PaneID}) {
		t.Fatalf("expected pane order [%s %s %s] after keeping Second branch, got %v", p1.PaneID, p2.PaneID, p3.PaneID, collectLayoutPaneIDs(&after2.Root))
	}
}

func TestOrderInvariantAnchorAndTabProfiles(t *testing.T) {
	profilesTOML := `
default_profile = "default"

[profiles.default]
placement = "down"
scope = "anchor"
size_ratio = 0.30
persist_on_hide = true
anchor_policy = "app_in_tab_or_tab_scope"
anchor_app_match = ["nvim", "vim"]

[profiles.cursorRight]
placement = "right"
scope = "tab"
size_ratio = 0.35
persist_on_hide = true
command = "python3 -c \"import time; time.sleep(30)\""
show_command_text = false
`
	assertCanonicalTree := func(t *testing.T, srv *mockHerdrServer, tabID, nvimPaneID string) (defaultPaneID, cursorPaneID string) {
		t.Helper()
		client := &RPCClient{socketPath: srv.sockPath}
		layout, err := client.ExportLayout(tabID)
		if err != nil {
			t.Fatalf("export layout: %v", err)
		}
		root := layout.Root
		if root.Type != "split" || root.Direction != "right" || root.Ratio != 0.65 {
			t.Fatalf("expected root split(right, 0.65), got %+v", root)
		}
		if root.Second == nil || root.Second.Type != "pane" {
			t.Fatalf("expected root.Second to be cursorRight pane, got %+v", root.Second)
		}
		if root.First == nil || root.First.Type != "split" || root.First.Direction != "down" || root.First.Ratio != 0.70 {
			t.Fatalf("expected root.First to be split(down, 0.70), got %+v", root.First)
		}
		if root.First.First == nil || root.First.First.PaneID != nvimPaneID {
			t.Fatalf("expected root.First.First to be nvim pane %s, got %+v", nvimPaneID, root.First.First)
		}
		if root.First.Second == nil || root.First.Second.Type != "pane" {
			t.Fatalf("expected root.First.Second to be default drawer pane, got %+v", root.First.Second)
		}
		return root.First.Second.PaneID, root.Second.PaneID
	}

	// Order A: default first, then cursorRight (while focused inside default drawer).
	t.Run("default_then_cursorRight", func(t *testing.T) {
		srv, _ := startTestEnv(t, profilesTOML)
		nvimPane := srv.addInitialPane("w1", "w1:t1", "term_nvim", "/tmp", true)
		srv.setProcesses(nvimPane.PaneID, []ForegroundProcess{{PID: 101, Name: "nvim", Cwd: "/tmp"}})

		if err := runAction("toggle", "", "", "right", "default", "workspace"); err != nil {
			t.Fatalf("toggle default: %v", err)
		}
		if err := runAction("toggle", "", "", "right", "cursorRight", "workspace"); err != nil {
			t.Fatalf("toggle cursorRight: %v", err)
		}
		assertCanonicalTree(t, srv, "w1:t1", nvimPane.PaneID)

		// Hide default while cursorRight stays open, then reopen default while focused on cursorRight.
		if err := runAction("toggle", "", "", "right", "default", "workspace"); err != nil {
			t.Fatalf("hide default: %v", err)
		}
		if err := runAction("toggle", "", "", "right", "default", "workspace"); err != nil {
			t.Fatalf("reopen default: %v", err)
		}
		assertCanonicalTree(t, srv, "w1:t1", nvimPane.PaneID)
	})

	// Order B: cursorRight first, then default (while focused inside cursorRight).
	t.Run("cursorRight_then_default", func(t *testing.T) {
		srv, _ := startTestEnv(t, profilesTOML)
		nvimPane := srv.addInitialPane("w1", "w1:t1", "term_nvim", "/tmp", true)
		srv.setProcesses(nvimPane.PaneID, []ForegroundProcess{{PID: 101, Name: "nvim", Cwd: "/tmp"}})

		if err := runAction("toggle", "", "", "right", "cursorRight", "workspace"); err != nil {
			t.Fatalf("toggle cursorRight: %v", err)
		}
		if err := runAction("toggle", "", "", "right", "default", "workspace"); err != nil {
			t.Fatalf("toggle default: %v", err)
		}
		assertCanonicalTree(t, srv, "w1:t1", nvimPane.PaneID)

		// Hide cursorRight while default stays open, then reopen cursorRight.
		if err := runAction("toggle", "", "", "right", "cursorRight", "workspace"); err != nil {
			t.Fatalf("hide cursorRight: %v", err)
		}
		if err := runAction("toggle", "", "", "right", "cursorRight", "workspace"); err != nil {
			t.Fatalf("reopen cursorRight: %v", err)
		}
		assertCanonicalTree(t, srv, "w1:t1", nvimPane.PaneID)
	})
}

func TestDrawerSplitHideUnhideAndPersistFalse(t *testing.T) {
	profilesTOML := `
default_profile = "default"

[profiles.default]
placement = "down"
scope = "anchor"
size_ratio = 0.30
persist_on_hide = true

[profiles.ephemeral]
placement = "up"
scope = "anchor"
size_ratio = 0.25
persist_on_hide = false
command = "echo EPHEMERAL_RUN"
show_command_text = true
`
	srv, _ := startTestEnv(t, profilesTOML)
	rootPane := srv.addInitialPane("w1", "w1:t1", "term_root", "/tmp", true)

	// Open default drawer and split it right.
	if err := runAction("open", rootPane.PaneID, "/tmp", "right", "default", "workspace"); err != nil {
		t.Fatalf("open default: %v", err)
	}
	if err := runAction("split", "", "/tmp", "right", "default", "workspace"); err != nil {
		t.Fatalf("split default: %v", err)
	}
	if got := len(listDaemonSessions()); got != 2 {
		t.Fatalf("expected 2 daemon sessions after split, got %d", got)
	}

	// Close (hide) default drawer; both sessions should persist.
	if err := runAction("close", "", "", "right", "default", "workspace"); err != nil {
		t.Fatalf("close default: %v", err)
	}
	if got := len(listDaemonSessions()); got != 2 {
		t.Fatalf("expected 2 persisted sessions while hidden, got %d", got)
	}

	// Reopen default drawer; nested split should be reconstructed.
	if err := runAction("open", rootPane.PaneID, "", "right", "default", "workspace"); err != nil {
		t.Fatalf("reopen default: %v", err)
	}
	client := &RPCClient{socketPath: srv.sockPath}
	layout, err := client.ExportLayout("w1:t1")
	if err != nil {
		t.Fatalf("export layout: %v", err)
	}
	if layout.Root.Type != "split" || layout.Root.Second == nil || layout.Root.Second.Type != "split" {
		t.Fatalf("expected reconstructed split drawer subtree, got %+v", layout.Root)
	}

	// Close default drawer before testing ephemeral profile.
	if err := runAction("close", "", "", "right", "default", "workspace"); err != nil {
		t.Fatalf("close default 2: %v", err)
	}

	// Open ephemeral profile (placement = "up", persist_on_hide = false, show_command_text = true).
	if err := runAction("toggle", rootPane.PaneID, "/tmp", "right", "ephemeral", "workspace"); err != nil {
		t.Fatalf("open ephemeral: %v", err)
	}
	if got := len(listDaemonSessions()); got != 3 {
		t.Fatalf("expected 3 daemon sessions (2 default + 1 ephemeral), got %d", got)
	}

	// Verify pane.send_text was called for show_command_text = true.
	srv.mu.Lock()
	totalSent := 0
	for _, list := range srv.sentText {
		for _, txt := range list {
			if strings.Contains(txt, "echo EPHEMERAL_RUN") {
				totalSent++
			}
		}
	}
	srv.mu.Unlock()
	if totalSent != 1 {
		t.Fatalf("expected pane.send_text for ephemeral command once, got %d", totalSent)
	}

	// Hide ephemeral profile; its session must be killed while default's 2 sessions survive.
	if err := runAction("toggle", rootPane.PaneID, "/tmp", "right", "ephemeral", "workspace"); err != nil {
		t.Fatalf("hide ephemeral: %v", err)
	}
	if got := len(listDaemonSessions()); got != 2 {
		t.Fatalf("expected 2 daemon sessions after hiding ephemeral profile, got %d", got)
	}
}

func TestAdoptNativeDrawerSplitsAndCleanup(t *testing.T) {
	srv, _ := startTestEnv(t, "")
	rootPane := srv.addInitialPane("w1", "w1:t1", "term_root", "/tmp", true)

	// Sync cwd and verify resolveAnchorCwd reads it.
	if err := runAction("sync-cwd", rootPane.PaneID, "/tmp", "right", "default", "workspace"); err != nil {
		t.Fatalf("sync-cwd: %v", err)
	}
	client := &RPCClient{socketPath: srv.sockPath}
	if got := resolveAnchorCwd(client, *rootPane, ""); got != "/tmp" {
		t.Fatalf("expected synced cwd /tmp, got %q", got)
	}

	// Open default drawer.
	if err := runAction("toggle", rootPane.PaneID, "", "right", "default", "workspace"); err != nil {
		t.Fatalf("open default: %v", err)
	}

	// Simulate a native unlabeled split inside the drawer pane.
	srv.mu.Lock()
	var drawerPaneID string
	for id, p := range srv.panes {
		if id != rootPane.PaneID {
			drawerPaneID = p.PaneID
			break
		}
	}
	nativeID := fmt.Sprintf("w1:p%d", srv.nextPaneNum)
	nativeTerm := fmt.Sprintf("term_%d", srv.nextPaneNum)
	srv.nextPaneNum++
	srv.panes[nativeID] = &PaneInfo{
		PaneID:        nativeID,
		TabID:         "w1:t1",
		WorkspaceID:   "w1",
		TerminalID:    nativeTerm,
		Label:         "", // Native split has empty label
		Cwd:           "/tmp",
		ForegroundCwd: "/tmp",
		Focused:       true,
	}
	srv.paneOrder = append(srv.paneOrder, nativeID)
	_, _ = replacePaneWithSplit(srv.tabs["w1:t1"], drawerPaneID, &LayoutNode{
		Type:      "split",
		Direction: "right",
		Ratio:     0.5,
		First:     &LayoutNode{Type: "pane", PaneID: drawerPaneID},
		Second:    &LayoutNode{Type: "pane", PaneID: nativeID},
	})
	srv.mu.Unlock()

	if err := adoptNativeDrawerSplits("w1:t1"); err != nil {
		t.Fatalf("adoptNativeDrawerSplits: %v", err)
	}
	if got := len(listDaemonSessions()); got != 2 {
		t.Fatalf("expected 2 daemon sessions after adopting native split, got %d", got)
	}

	// Test workspace cleanup.
	if err := runAction("cleanup", rootPane.PaneID, "", "right", "default", "workspace"); err != nil {
		t.Fatalf("cleanup workspace: %v", err)
	}
	if got := len(listDaemonSessions()); got != 0 {
		t.Fatalf("expected 0 daemon sessions after workspace cleanup, got %d", got)
	}

	// Reopen and test cleanup all (which also shuts down the daemon).
	if err := runAction("toggle", rootPane.PaneID, "", "right", "default", "workspace"); err != nil {
		t.Fatalf("reopen before cleanup all: %v", err)
	}
	if err := runAction("cleanup", rootPane.PaneID, "", "right", "default", "all"); err != nil {
		t.Fatalf("cleanup all: %v", err)
	}
	if isDaemonAlive() {
		t.Fatalf("expected daemon to be shut down after cleanup --scope all")
	}
}

func TestChooseAnchorPaneFallbackToTabScope(t *testing.T) {
	profilesTOML := `
[profiles.default]
placement = "down"
scope = "anchor"
size_ratio = 0.30
anchor_policy = "app_in_tab_or_tab_scope"
anchor_app_match = ["nvim", "vim"]
`
	srv, _ := startTestEnv(t, profilesTOML)
	p1 := srv.addInitialPane("w1", "w1:t1", "term1", "/tmp", true)
	p2 := srv.addInitialPane("w1", "w1:t1", "term2", "/tmp", false)
	srv.setProcesses(p1.PaneID, []ForegroundProcess{{PID: 10, Name: "zsh", Cwd: "/tmp"}})
	srv.setProcesses(p2.PaneID, []ForegroundProcess{{PID: 11, Name: "zsh", Cwd: "/tmp"}})

	// Neither p1 nor p2 runs nvim/vim, and there are 2 non-plugin panes -> should fall back to tab scope!
	if err := runAction("toggle", p1.PaneID, "/tmp", "right", "default", "workspace"); err != nil {
		t.Fatalf("toggle default fallback to tab scope: %v", err)
	}
	client := &RPCClient{socketPath: srv.sockPath}
	layout, err := client.ExportLayout("w1:t1")
	if err != nil {
		t.Fatalf("export layout: %v", err)
	}
	if layout.Root.Type != "split" || layout.Root.Direction != "down" || layout.Root.First == nil || layout.Root.First.Type != "split" {
		t.Fatalf("expected root split(down) wrapping split(p1, p2), got %+v", layout.Root)
	}
}

func TestOrphanGCStateNormalizationAndEdgeActions(t *testing.T) {
	srv, _ := startTestEnv(t, "")
	p1 := srv.addInitialPane("w1", "w1:t1", "term1", "/tmp", true)
	p2 := srv.addInitialPane("w1", "w1:t2", "term2", "/tmp", true)

	// Test legacy StateFile normalization (DrawerRatio -> UtilityRatio, legacy key without ::profile).
	sf := &StateFile{
		Tabs: map[string]*TabDrawerState{
			"w1:tlegacy": {
				DrawerRatio: 0.65,
			},
		},
	}
	normalizeStateFile(sf)
	norm := sf.Tabs["w1:tlegacy::default"]
	if norm == nil || norm.TabID != "w1:tlegacy" || norm.ProfileID != "default" {
		t.Fatalf("expected normalized legacy entry, got %+v", sf.Tabs)
	}
	if norm.UtilityRatio < 0.34 || norm.UtilityRatio > 0.36 {
		t.Fatalf("expected UtilityRatio ~0.35 from DrawerRatio 0.65, got %v", norm.UtilityRatio)
	}

	// Calling "close" when already closed is a no-op.
	if err := runAction("close", p1.PaneID, "/tmp", "right", "default", "workspace"); err != nil {
		t.Fatalf("close when already hidden: %v", err)
	}

	// Calling "split" when closed opens the drawer.
	if err := runAction("split", p1.PaneID, "/tmp", "down", "default", "workspace"); err != nil {
		t.Fatalf("split when hidden: %v", err)
	}

	// Calling "open" when already open focuses the drawer pane without creating a duplicate session.
	sessionsBefore := len(listDaemonSessions())
	if err := runAction("open", p1.PaneID, "/tmp", "right", "default", "workspace"); err != nil {
		t.Fatalf("open when already visible: %v", err)
	}
	if got := len(listDaemonSessions()); got != sessionsBefore {
		t.Fatalf("expected session count %d after open on visible drawer, got %d", sessionsBefore, got)
	}

	// Hide drawer on t1, then close t1's anchor pane in Herdr; next action on t2 should GC t1's orphaned session.
	if err := runAction("close", p1.PaneID, "/tmp", "right", "default", "workspace"); err != nil {
		t.Fatalf("hide t1 drawer: %v", err)
	}
	client := &RPCClient{socketPath: srv.sockPath}
	_ = client.ClosePane(p1.PaneID)

	// Running an action on t2 triggers gcOrphanedDrawers, which reaps t1's orphaned session.
	if err := runAction("open", p2.PaneID, "/tmp", "right", "default", "workspace"); err != nil {
		t.Fatalf("open t2 drawer: %v", err)
	}
	snap, err := readStateSnapshot()
	if err != nil {
		t.Fatalf("readStateSnapshot: %v", err)
	}
	if _, exists := snap.Tabs[stateKey("w1:t1", "default")]; exists {
		t.Fatalf("expected closed tab w1:t1 to be garbage collected from state.json")
	}
	if got := len(listDaemonSessions()); got != 1 {
		t.Fatalf("expected only t2's 1 session to remain after orphan GC, got %d", got)
	}

	// Unknown action should return an error.
	if err := runAction("nonexistent-action", p2.PaneID, "/tmp", "right", "default", "workspace"); err == nil {
		t.Fatalf("expected error for unknown action")
	}

	// Test expandNvimConfigPath and displayHomePath helpers.
	home, _ := os.UserHomeDir()
	if got := displayHomePath(home); got != "~" {
		t.Fatalf("expected ~ for home, got %q", got)
	}
	if got := expandNvimConfigPath("~/nvim-test/custom.lua", "/def"); got != filepath.Join(home, "nvim-test", "custom.lua") {
		t.Fatalf("unexpected expanded ~/nvim-test/custom.lua: %q", got)
	}
	tmpDir := t.TempDir()
	if got := expandNvimConfigPath(tmpDir, "/def"); got != filepath.Join(tmpDir, "herdr-drawer.lua") {
		t.Fatalf("unexpected expanded dir %s: %q", tmpDir, got)
	}
}

func TestDaemonWatchdogReconcilePreservesHiddenSession(t *testing.T) {
	srv := newMockHerdrServer(t)
	stateDirPath := filepath.Join("/tmp", "hd-wd-"+newSessionID())
	cfgDirPath := t.TempDir()
	if err := os.MkdirAll(stateDirPath, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	t.Setenv("HERDR_SOCKET_PATH", srv.sockPath)
	t.Setenv("HERDR_DRAWER_STATE_DIR", stateDirPath)
	t.Setenv("HERDR_DRAWER_CONFIG_DIR", cfgDirPath)
	t.Setenv("HERDR_DRAWER_NO_WATCHDOG", "") // Enable watchHerdrEvents + 350ms timer!

	go func() {
		_ = runDaemon()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !isDaemonAlive() {
		time.Sleep(15 * time.Millisecond)
	}
	if !isDaemonAlive() {
		t.Fatalf("daemon did not start")
	}
	t.Cleanup(func() {
		shutdownDaemon()
		_ = os.RemoveAll(stateDirPath)
	})

	p1 := srv.addInitialPane("w1", "w1:t1", "term1", "/tmp", true)

	// Open drawer.
	if err := runAction("toggle", p1.PaneID, "/tmp", "right", "default", "workspace"); err != nil {
		t.Fatalf("open drawer: %v", err)
	}
	sessions := listDaemonSessions()
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %v", sessions)
	}
	sessID := sessions[0]

	// Attach and then disconnect while hiding drawer, triggering notifyDisconnect -> 350ms reconcileSessions.
	conn, err := net.DialTimeout("unix", daemonSocketPath(), time.Second)
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	_, _ = conn.Write([]byte(`{"type":"attach","session_id":"` + sessID + `","rows":24,"cols":80}` + "\n"))
	time.Sleep(50 * time.Millisecond)

	if err := runAction("toggle", p1.PaneID, "/tmp", "right", "default", "workspace"); err != nil {
		t.Fatalf("hide drawer: %v", err)
	}
	_ = conn.Close()

	// Wait >450ms so the 350ms disconnect reconciliation timer fires.
	time.Sleep(450 * time.Millisecond)
	if !isSessionAlive(sessID) {
		t.Fatalf("expected hidden drawer session %s to survive 350ms watchdog reconciliation", sessID)
	}
}

func TestWatchDrawerAgentStatusAndOrdering(t *testing.T) {
	srv := newMockHerdrServer(t)
	t.Setenv("HERDR_SOCKET_PATH", srv.sockPath)

	outputSignal := make(chan struct{}, 1)
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		watchDrawerAgentStatus("w4:p5F", "cursor", "sess-123", "/usr/local/bin/cursor-agent --resume abc", outputSignal, stopCh)
	}()

	waitForReports := func(want int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			srv.mu.Lock()
			got := len(srv.agentReportEvents)
			srv.mu.Unlock()
			if got >= want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		srv.mu.Lock()
		defer srv.mu.Unlock()
		t.Fatalf("timed out waiting for %d agent reports, got %v", want, srv.agentReportEvents)
	}

	waitForReports(1)
	time.Sleep(30 * time.Millisecond)
	srv.mu.Lock()
	if len(srv.agentSessionEvents) != 1 || srv.agentSessionEvents[0] != "w4:p5F:cursor:sess-123:cursor-agent --resume abc" {
		srv.mu.Unlock()
		t.Fatalf("expected pane.report_agent_session to succeed after pane.report_agent, got %v", srv.agentSessionEvents)
	}
	srv.mu.Unlock()

	// Output while explain state is still "idle" (e.g. typing in prompt box) must NOT flip status to working.
	select {
	case outputSignal <- struct{}{}:
	default:
	}
	time.Sleep(220 * time.Millisecond)
	srv.mu.Lock()
	if len(srv.agentReportEvents) != 1 {
		srv.mu.Unlock()
		t.Fatalf("expected typing output in idle state to not trigger working transition, got %v", srv.agentReportEvents)
	}
	srv.explainState = "working"
	srv.explainReason = "stop_hint_working"
	srv.mu.Unlock()

	select {
	case outputSignal <- struct{}{}:
	default:
	}
	waitForReports(2)

	srv.mu.Lock()
	srv.explainState = "blocked"
	srv.explainReason = "approval_prompt"
	srv.mu.Unlock()
	select {
	case outputSignal <- struct{}{}:
	default:
	}
	waitForReports(3)

	srv.mu.Lock()
	srv.explainState = "idle"
	srv.explainReason = "prompt_ready"
	srv.mu.Unlock()
	select {
	case outputSignal <- struct{}{}:
	default:
	}
	waitForReports(4)

	close(stopCh)
	<-doneCh

	srv.mu.Lock()
	got := append([]string(nil), srv.agentReportEvents...)
	srv.mu.Unlock()
	want := []string{
		"w4:p5F:cursor:idle:",
		"w4:p5F:cursor:working:stop_hint_working",
		"w4:p5F:cursor:blocked:approval_prompt",
		"w4:p5F:cursor:idle:prompt_ready",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected agent report events:\ngot:  %v\nwant: %v", got, want)
	}
}




package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	pluginID           = "herdr-drawer"
	drawerEntrypointID = "drawer"
	defaultDrawerRatio = 0.70
)

type RPCClient struct {
	socketPath string
	reqCounter uint64
}

type rpcRequest struct {
	ID     string      `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

type rpcErrorEnvelope struct {
	ID    string `json:"id"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

type PaneInfo struct {
	PaneID        string `json:"pane_id"`
	TabID         string `json:"tab_id"`
	WorkspaceID   string `json:"workspace_id"`
	TerminalID    string `json:"terminal_id"`
	Cwd           string `json:"cwd"`
	ForegroundCwd string `json:"foreground_cwd"`
	Focused       bool   `json:"focused"`
}

type LayoutExport struct {
	WorkspaceID   string     `json:"workspace_id"`
	TabID         string     `json:"tab_id"`
	Zoomed        bool       `json:"zoomed"`
	FocusedPaneID string     `json:"focused_pane_id"`
	Root          LayoutNode `json:"root"`
}

type LayoutNode struct {
	Type      string      `json:"type"`
	PaneID    string      `json:"pane_id,omitempty"`
	Cwd       string      `json:"cwd,omitempty"`
	Direction string      `json:"direction,omitempty"`
	Ratio     float64     `json:"ratio,omitempty"`
	First     *LayoutNode `json:"first,omitempty"`
	Second    *LayoutNode `json:"second,omitempty"`
}

type ProcessInfo struct {
	PaneID              string              `json:"pane_id"`
	ForegroundProcesses []ForegroundProcess `json:"foreground_processes"`
}

type ForegroundProcess struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
	Cwd  string `json:"cwd"`
}

type DrawerNode struct {
	Type      string      `json:"type"` // "leaf" or "split"
	SessionID string      `json:"session_id,omitempty"`
	Cwd       string      `json:"cwd,omitempty"`
	Direction string      `json:"direction,omitempty"`
	Ratio     float64     `json:"ratio,omitempty"`
	First     *DrawerNode `json:"first,omitempty"`
	Second    *DrawerNode `json:"second,omitempty"`
}

type TabDrawerState struct {
	TabID            string            `json:"tab_id"`
	WorkspaceID      string            `json:"workspace_id"`
	AnchorTerminalID string            `json:"anchor_terminal_id"`
	DrawerRatio      float64           `json:"drawer_ratio"`
	Visible          bool              `json:"visible"`
	FocusedSessionID string            `json:"focused_session_id"`
	TermToSession    map[string]string `json:"term_to_session"`
	Tree             *DrawerNode       `json:"tree,omitempty"`
}

type StateFile struct {
	Tabs map[string]*TabDrawerState `json:"tabs"`
}

func resolveSocketPath() (string, error) {
	if s := os.Getenv("HERDR_SOCKET_PATH"); s != "" {
		return s, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(home, ".config", "herdr", "herdr.sock")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	return "", errors.New("HERDR_SOCKET_PATH is not set and ~/.config/herdr/herdr.sock does not exist")
}

func (c *RPCClient) Call(method string, params interface{}, out interface{}) error {
	conn, err := net.DialTimeout("unix", c.socketPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect %s: %w", c.socketPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	id := fmt.Sprintf("drawer-%d", atomic.AddUint64(&c.reqCounter, 1))
	if params == nil {
		params = map[string]interface{}{}
	}
	reqBytes, err := json.Marshal(rpcRequest{
		ID:     id,
		Method: method,
		Params: params,
	})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(reqBytes, '\n')); err != nil {
		return err
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
	}

	var env rpcErrorEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return fmt.Errorf("decode %s envelope: %w", method, err)
	}
	if env.Error != nil {
		return fmt.Errorf("rpc %s error (%s): %s", method, env.Error.Code, env.Error.Message)
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}
	return nil
}

func (c *RPCClient) ListPanes() ([]PaneInfo, error) {
	var res struct {
		Panes []PaneInfo `json:"panes"`
	}
	if err := c.Call("pane.list", map[string]interface{}{}, &res); err != nil {
		return nil, err
	}
	return res.Panes, nil
}

func (c *RPCClient) ExportLayout(tabID string) (*LayoutExport, error) {
	var res struct {
		Layout LayoutExport `json:"layout"`
	}
	if err := c.Call("layout.export", map[string]interface{}{"tab_id": tabID}, &res); err != nil {
		return nil, err
	}
	return &res.Layout, nil
}

func (c *RPCClient) GetProcessInfo(paneID string) (*ProcessInfo, error) {
	var res struct {
		ProcessInfo ProcessInfo `json:"process_info"`
	}
	if err := c.Call("pane.process_info", map[string]interface{}{"pane_id": paneID}, &res); err != nil {
		return nil, err
	}
	return &res.ProcessInfo, nil
}

func (c *RPCClient) OpenPluginSplitPane(targetPaneID, direction, cwd, sessionID string, focus bool) (*PaneInfo, error) {
	params := map[string]interface{}{
		"plugin_id":      pluginID,
		"entrypoint":     drawerEntrypointID,
		"placement":      "split",
		"direction":      direction,
		"target_pane_id": targetPaneID,
		"focus":          focus,
		"env": map[string]string{
			"HERDR_DRAWER_SESSION_ID": sessionID,
			"HERDR_DRAWER_CWD":        cwd,
		},
	}
	if cwd != "" {
		params["cwd"] = cwd
	}
	var res struct {
		PluginPane struct {
			Pane PaneInfo `json:"pane"`
		} `json:"plugin_pane"`
	}
	if err := c.Call("plugin.pane.open", params, &res); err != nil {
		return nil, err
	}
	return &res.PluginPane.Pane, nil
}

func (c *RPCClient) SetSplitRatio(tabID string, path []bool, ratio float64) error {
	if path == nil {
		path = make([]bool, 0)
	}
	params := map[string]interface{}{
		"tab_id": tabID,
		"path":   path,
		"ratio":  ratio,
	}
	return c.Call("layout.set_split_ratio", params, nil)
}

func (c *RPCClient) ClosePane(paneID string) error {
	return c.Call("pane.close", map[string]interface{}{"pane_id": paneID}, nil)
}

func (c *RPCClient) FocusPane(paneID string) error {
	return c.Call("pane.focus", map[string]interface{}{"pane_id": paneID}, nil)
}

func stateDir() string {
	return fmt.Sprintf("/tmp/herdr-drawer-%d", os.Getuid())
}

func withStateLock(fn func(*StateFile) error) error {
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
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

	statePath := filepath.Join(dir, "state.json")
	sf := &StateFile{Tabs: map[string]*TabDrawerState{}}
	if data, err := os.ReadFile(statePath); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, sf)
		if sf.Tabs == nil {
			sf.Tabs = map[string]*TabDrawerState{}
		}
	}

	if err := fn(sf); err != nil {
		return err
	}

	out, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := statePath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, statePath)
}

func sanitizePaneID(paneID string) string {
	return strings.ReplaceAll(paneID, ":", "_")
}

func writeSyncedCwd(paneID, cwd string) error {
	if paneID == "" || cwd == "" {
		return nil
	}
	cwdDir := filepath.Join(stateDir(), "cwd")
	if err := os.MkdirAll(cwdDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cwdDir, sanitizePaneID(paneID)), []byte(cwd), 0o600)
}

func readSyncedCwd(paneID string) string {
	if paneID == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(stateDir(), "cwd", sanitizePaneID(paneID)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func resolveAnchorCwd(client *RPCClient, anchor PaneInfo, explicitCwd string) string {
	if explicitCwd != "" {
		return explicitCwd
	}
	if synced := readSyncedCwd(anchor.PaneID); synced != "" {
		if info, err := os.Stat(synced); err == nil && info.IsDir() {
			return synced
		}
	}
	if procInfo, err := client.GetProcessInfo(anchor.PaneID); err == nil && procInfo != nil {
		for _, proc := range procInfo.ForegroundProcesses {
			if proc.Cwd != "" {
				return proc.Cwd
			}
		}
	}
	if anchor.ForegroundCwd != "" {
		return anchor.ForegroundCwd
	}
	return anchor.Cwd
}

func layoutContainsPane(node *LayoutNode, paneID string) bool {
	if node == nil {
		return false
	}
	if node.Type == "pane" {
		return node.PaneID == paneID
	}
	return layoutContainsPane(node.First, paneID) || layoutContainsPane(node.Second, paneID)
}

func layoutContainsAnyTerm(node *LayoutNode, paneToTerm map[string]string, termToSession map[string]string) bool {
	if node == nil || len(termToSession) == 0 {
		return false
	}
	if node.Type == "pane" {
		termID := paneToTerm[node.PaneID]
		return termID != "" && termToSession[termID] != ""
	}
	return layoutContainsAnyTerm(node.First, paneToTerm, termToSession) || layoutContainsAnyTerm(node.Second, paneToTerm, termToSession)
}

func findDrawerSplit(root *LayoutNode, anchorPaneID string, paneToTerm map[string]string, termToSession map[string]string, allowFallback bool) *LayoutNode {
	if root == nil || root.Type != "split" {
		return nil
	}
	if found := findDrawerSplit(root.First, anchorPaneID, paneToTerm, termToSession, allowFallback); found != nil {
		return found
	}
	if found := findDrawerSplit(root.Second, anchorPaneID, paneToTerm, termToSession, allowFallback); found != nil {
		return found
	}
	if root.Direction == "down" && layoutContainsPane(root.First, anchorPaneID) {
		if layoutContainsAnyTerm(root.Second, paneToTerm, termToSession) || allowFallback {
			return root
		}
	}
	return nil
}

func findSplitPath(node *LayoutNode, firstPaneID, secondPaneID string, prefix []bool) ([]bool, bool) {
	if node == nil || node.Type != "split" {
		return nil, false
	}
	leftPath := append(append([]bool{}, prefix...), false)
	rightPath := append(append([]bool{}, prefix...), true)
	if p, ok := findSplitPath(node.First, firstPaneID, secondPaneID, leftPath); ok {
		return p, true
	}
	if p, ok := findSplitPath(node.Second, firstPaneID, secondPaneID, rightPath); ok {
		return p, true
	}
	if layoutContainsPane(node.First, firstPaneID) && layoutContainsPane(node.Second, secondPaneID) {
		out := make([]bool, len(prefix))
		copy(out, prefix)
		return out, true
	}
	return nil, false
}

func layoutToDrawerTree(node *LayoutNode, paneByID map[string]PaneInfo, termToSession map[string]string) *DrawerNode {
	if node == nil {
		return nil
	}
	if node.Type == "pane" {
		p, ok := paneByID[node.PaneID]
		if !ok {
			return nil
		}
		sessID := termToSession[p.TerminalID]
		if sessID == "" {
			return nil
		}
		cwd := p.ForegroundCwd
		if cwd == "" {
			cwd = p.Cwd
		}
		return &DrawerNode{
			Type:      "leaf",
			SessionID: sessID,
			Cwd:       cwd,
		}
	}
	first := layoutToDrawerTree(node.First, paneByID, termToSession)
	second := layoutToDrawerTree(node.Second, paneByID, termToSession)
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return &DrawerNode{
		Type:      "split",
		Direction: node.Direction,
		Ratio:     node.Ratio,
		First:     first,
		Second:    second,
	}
}

func collectLayoutPaneIDs(node *LayoutNode) []string {
	if node == nil {
		return nil
	}
	if node.Type == "pane" {
		if node.PaneID != "" {
			return []string{node.PaneID}
		}
		return nil
	}
	return append(collectLayoutPaneIDs(node.First), collectLayoutPaneIDs(node.Second)...)
}

func collectDrawerSessions(node *DrawerNode, out map[string]bool) {
	if node == nil {
		return
	}
	if node.Type == "leaf" {
		if node.SessionID != "" {
			out[node.SessionID] = true
		}
		return
	}
	collectDrawerSessions(node.First, out)
	collectDrawerSessions(node.Second, out)
}

func collectDrawerLeavesOrdered(node *DrawerNode) []string {
	if node == nil {
		return nil
	}
	if node.Type == "leaf" {
		if node.SessionID != "" {
			return []string{node.SessionID}
		}
		return nil
	}
	return append(collectDrawerLeavesOrdered(node.First), collectDrawerLeavesOrdered(node.Second)...)
}

func pruneDrawerTree(node *DrawerNode, isAlive func(string) bool) *DrawerNode {
	if node == nil {
		return nil
	}
	if node.Type == "leaf" {
		if node.SessionID == "" || !isAlive(node.SessionID) {
			return nil
		}
		return &DrawerNode{
			Type:      "leaf",
			SessionID: node.SessionID,
			Cwd:       node.Cwd,
		}
	}
	first := pruneDrawerTree(node.First, isAlive)
	second := pruneDrawerTree(node.Second, isAlive)
	if first != nil && second != nil {
		return &DrawerNode{
			Type:      "split",
			Direction: node.Direction,
			Ratio:     node.Ratio,
			First:     first,
			Second:    second,
		}
	}
	if first != nil {
		return first
	}
	return second
}

func firstLeafNode(node *DrawerNode) *DrawerNode {
	if node == nil {
		return nil
	}
	if node.Type == "leaf" {
		return node
	}
	if l := firstLeafNode(node.First); l != nil {
		return l
	}
	return firstLeafNode(node.Second)
}

func firstLeafSession(node *DrawerNode) string {
	if l := firstLeafNode(node); l != nil {
		return l.SessionID
	}
	return ""
}

func firstLayoutPaneID(node *LayoutNode) string {
	if node == nil {
		return ""
	}
	if node.Type == "pane" {
		return node.PaneID
	}
	if id := firstLayoutPaneID(node.First); id != "" {
		return id
	}
	return firstLayoutPaneID(node.Second)
}

func openDrawerPaneWithRatio(client *RPCClient, tabID, targetPaneID, direction string, ratio float64, sessionID, cwd string, focus bool) (*PaneInfo, error) {
	_ = ensureSessionHost(sessionID, cwd, 24, 80)
	pane, err := client.OpenPluginSplitPane(targetPaneID, direction, cwd, sessionID, focus)
	if err != nil {
		return nil, err
	}
	if ratio >= 0.1 && ratio <= 0.9 && math.Abs(ratio-0.5) > 0.005 {
		if layout, err := client.ExportLayout(tabID); err == nil {
			if path, ok := findSplitPath(&layout.Root, targetPaneID, pane.PaneID, nil); ok {
				_ = client.SetSplitRatio(tabID, path, ratio)
			}
		}
	}
	return pane, nil
}

func reconstructSplitsPreOrder(client *RPCClient, tabID string, node *DrawerNode, sessionToPane map[string]PaneInfo, termToSession map[string]string) error {
	if node == nil || node.Type != "split" {
		return nil
	}
	leftLeaf := firstLeafNode(node.First)
	rightLeaf := firstLeafNode(node.Second)
	if leftLeaf == nil || rightLeaf == nil {
		return nil
	}
	targetPane, ok := sessionToPane[leftLeaf.SessionID]
	if !ok {
		return fmt.Errorf("missing open pane for session %s", leftLeaf.SessionID)
	}
	ratio := node.Ratio
	if ratio < 0.1 || ratio > 0.9 {
		ratio = 0.5
	}
	dir := node.Direction
	if dir != "right" && dir != "down" {
		dir = "right"
	}
	newPane, err := openDrawerPaneWithRatio(client, tabID, targetPane.PaneID, dir, ratio, rightLeaf.SessionID, rightLeaf.Cwd, false)
	if err != nil {
		return err
	}
	sessionToPane[rightLeaf.SessionID] = *newPane
	termToSession[newPane.TerminalID] = rightLeaf.SessionID

	if err := reconstructSplitsPreOrder(client, tabID, node.First, sessionToPane, termToSession); err != nil {
		return err
	}
	return reconstructSplitsPreOrder(client, tabID, node.Second, sessionToPane, termToSession)
}

func findTargetPane(panes []PaneInfo, explicitPaneID string) (*PaneInfo, error) {
	if explicitPaneID != "" {
		for i := range panes {
			if panes[i].PaneID == explicitPaneID {
				return &panes[i], nil
			}
		}
	}
	if envPane := os.Getenv("HERDR_PANE_ID"); envPane != "" {
		for i := range panes {
			if panes[i].PaneID == envPane {
				return &panes[i], nil
			}
		}
	}
	for i := range panes {
		if panes[i].Focused {
			return &panes[i], nil
		}
	}
	if len(panes) > 0 {
		return &panes[0], nil
	}
	return nil, errors.New("no active pane found")
}

func gcOrphanedDrawers(client *RPCClient, sf *StateFile, panes []PaneInfo, termToPane map[string]PaneInfo) {
	activeTabs := make(map[string]bool, len(panes))
	for _, p := range panes {
		activeTabs[p.TabID] = true
	}
	for tabID, ts := range sf.Tabs {
		anchorPane, anchorAlive := termToPane[ts.AnchorTerminalID]
		if !activeTabs[tabID] || (ts.AnchorTerminalID != "" && (!anchorAlive || anchorPane.TabID != tabID)) {
			sessions := map[string]bool{}
			collectDrawerSessions(ts.Tree, sessions)
			for termID, sessID := range ts.TermToSession {
				if sessID != "" {
					sessions[sessID] = true
				}
				if p, ok := termToPane[termID]; ok {
					_ = client.ClosePane(p.PaneID)
				}
			}
			for sessID := range sessions {
				killSession(sessID)
			}
			delete(sf.Tabs, tabID)
		}
	}
}

func hideDrawer(client *RPCClient, ts *TabDrawerState, layout *LayoutExport, drawerSplit *LayoutNode, currentPane *PaneInfo, paneByID map[string]PaneInfo, paneToTerm map[string]string, termToPane map[string]PaneInfo) error {
	tree := layoutToDrawerTree(drawerSplit.Second, paneByID, ts.TermToSession)
	if tree == nil {
		return errors.New("drawer split had no valid panes")
	}

	knownSessions := map[string]bool{}
	collectDrawerSessions(tree, knownSessions)

	ts.DrawerRatio = drawerSplit.Ratio
	ts.Tree = tree
	ts.Visible = false

	if currentPane != nil {
		if sessID := ts.TermToSession[currentPane.TerminalID]; knownSessions[sessID] {
			ts.FocusedSessionID = sessID
		}
	}
	if ts.FocusedSessionID == "" || !knownSessions[ts.FocusedSessionID] {
		if focusedTerm := paneToTerm[layout.FocusedPaneID]; focusedTerm != "" {
			if sessID := ts.TermToSession[focusedTerm]; knownSessions[sessID] {
				ts.FocusedSessionID = sessID
			}
		}
	}
	if ts.FocusedSessionID == "" || !knownSessions[ts.FocusedSessionID] {
		ts.FocusedSessionID = firstLeafSession(tree)
	}

	var focusPaneID string
	if anchorPane, ok := termToPane[ts.AnchorTerminalID]; ok && anchorPane.TabID == ts.TabID {
		focusPaneID = anchorPane.PaneID
	} else if drawerSplit.First != nil {
		focusPaneID = firstLayoutPaneID(drawerSplit.First)
	}
	if focusPaneID != "" {
		_ = client.FocusPane(focusPaneID)
	}

	drawerPaneIDs := collectLayoutPaneIDs(drawerSplit.Second)
	for _, paneID := range drawerPaneIDs {
		_ = client.ClosePane(paneID)
	}
	ts.TermToSession = map[string]string{}

	return nil
}

func showDrawer(client *RPCClient, ts *TabDrawerState, currentPane *PaneInfo, explicitCwd string, termToPane map[string]PaneInfo) error {
	anchorPane := *currentPane
	if ts.AnchorTerminalID != "" {
		if p, ok := termToPane[ts.AnchorTerminalID]; ok && p.TabID == currentPane.TabID {
			anchorPane = p
		}
	}
	ts.AnchorTerminalID = anchorPane.TerminalID
	ts.TabID = anchorPane.TabID
	ts.WorkspaceID = anchorPane.WorkspaceID
	if ts.DrawerRatio < 0.1 || ts.DrawerRatio > 0.9 {
		ts.DrawerRatio = defaultDrawerRatio
	}
	ts.TermToSession = map[string]string{}

	prunedTree := pruneDrawerTree(ts.Tree, isSessionAlive)
	if prunedTree == nil {
		cwd := resolveAnchorCwd(client, anchorPane, explicitCwd)
		sessionID := newSessionID()
		newPane, err := openDrawerPaneWithRatio(client, anchorPane.TabID, anchorPane.PaneID, "down", ts.DrawerRatio, sessionID, cwd, true)
		if err != nil {
			return err
		}
		ts.Tree = &DrawerNode{
			Type:      "leaf",
			SessionID: sessionID,
			Cwd:       cwd,
		}
		ts.TermToSession[newPane.TerminalID] = sessionID
		ts.FocusedSessionID = sessionID
		ts.Visible = true
		return nil
	}

	rootLeaf := firstLeafNode(prunedTree)
	if rootLeaf == nil {
		return errors.New("drawer tree has no leaf session")
	}
	if rootLeaf.Cwd == "" {
		rootLeaf.Cwd = resolveAnchorCwd(client, anchorPane, explicitCwd)
	}

	rootPane, err := openDrawerPaneWithRatio(client, ts.TabID, anchorPane.PaneID, "down", ts.DrawerRatio, rootLeaf.SessionID, rootLeaf.Cwd, false)
	if err != nil {
		return err
	}

	sessionToPane := map[string]PaneInfo{
		rootLeaf.SessionID: *rootPane,
	}
	ts.TermToSession[rootPane.TerminalID] = rootLeaf.SessionID

	if err := reconstructSplitsPreOrder(client, ts.TabID, prunedTree, sessionToPane, ts.TermToSession); err != nil {
		return err
	}

	knownSessions := map[string]bool{}
	collectDrawerSessions(prunedTree, knownSessions)
	ts.Tree = prunedTree
	ts.Visible = true

	focusSess := ts.FocusedSessionID
	if focusSess == "" || !knownSessions[focusSess] {
		focusSess = rootLeaf.SessionID
		ts.FocusedSessionID = focusSess
	}
	if focusPane, ok := sessionToPane[focusSess]; ok {
		_ = client.FocusPane(focusPane.PaneID)
	}

	return nil
}

func runAction(action, explicitPaneID, explicitCwd, splitDir string) error {
	sock, err := resolveSocketPath()
	if err != nil {
		return err
	}
	client := &RPCClient{socketPath: sock}

	if explicitPaneID != "" && explicitCwd != "" {
		_ = writeSyncedCwd(explicitPaneID, explicitCwd)
	}

	if action == "sync-cwd" {
		return nil
	}

	return withStateLock(func(sf *StateFile) error {
		panes, err := client.ListPanes()
		if err != nil {
			return err
		}
		paneByID := make(map[string]PaneInfo, len(panes))
		paneToTerm := make(map[string]string, len(panes))
		termToPane := make(map[string]PaneInfo, len(panes))
		for _, p := range panes {
			paneByID[p.PaneID] = p
			paneToTerm[p.PaneID] = p.TerminalID
			termToPane[p.TerminalID] = p
		}

		gcOrphanedDrawers(client, sf, panes, termToPane)

		currentPane, err := findTargetPane(panes, explicitPaneID)
		if err != nil {
			return err
		}

		tabID := currentPane.TabID
		ts, ok := sf.Tabs[tabID]
		if !ok {
			ts = &TabDrawerState{
				TabID:         tabID,
				WorkspaceID:   currentPane.WorkspaceID,
				DrawerRatio:   defaultDrawerRatio,
				TermToSession: map[string]string{},
			}
			sf.Tabs[tabID] = ts
		}
		if ts.TermToSession == nil {
			ts.TermToSession = map[string]string{}
		}

		layout, err := client.ExportLayout(tabID)
		if err != nil {
			return err
		}

		var anchorPaneID string
		if ts.AnchorTerminalID != "" {
			if ap, exists := termToPane[ts.AnchorTerminalID]; exists && ap.TabID == tabID {
				anchorPaneID = ap.PaneID
			}
		}

		var drawerSplit *LayoutNode
		if anchorPaneID != "" {
			drawerSplit = findDrawerSplit(&layout.Root, anchorPaneID, paneToTerm, ts.TermToSession, false)
		}
		isCurrentlyVisible := drawerSplit != nil

		switch action {
		case "toggle":
			if isCurrentlyVisible {
				return hideDrawer(client, ts, layout, drawerSplit, currentPane, paneByID, paneToTerm, termToPane)
			}
			ts.Visible = false
			return showDrawer(client, ts, currentPane, explicitCwd, termToPane)

		case "open":
			if isCurrentlyVisible {
				tree := layoutToDrawerTree(drawerSplit.Second, paneByID, ts.TermToSession)
				knownSessions := map[string]bool{}
				collectDrawerSessions(tree, knownSessions)
				targetSess := ts.FocusedSessionID
				if targetSess == "" || !knownSessions[targetSess] {
					targetSess = firstLeafSession(tree)
				}
				for termID, sessID := range ts.TermToSession {
					if sessID == targetSess {
						if p, ok := termToPane[termID]; ok {
							return client.FocusPane(p.PaneID)
						}
					}
				}
				return nil
			}
			ts.Visible = false
			return showDrawer(client, ts, currentPane, explicitCwd, termToPane)

		case "close":
			if !isCurrentlyVisible {
				ts.Visible = false
				return nil
			}
			return hideDrawer(client, ts, layout, drawerSplit, currentPane, paneByID, paneToTerm, termToPane)

		case "split":
			if !isCurrentlyVisible {
				return showDrawer(client, ts, currentPane, explicitCwd, termToPane)
			}
			tree := layoutToDrawerTree(drawerSplit.Second, paneByID, ts.TermToSession)
			knownSessions := map[string]bool{}
			collectDrawerSessions(tree, knownSessions)

			targetSplitPane := *currentPane
			if ts.TermToSession[currentPane.TerminalID] == "" {
				focusSess := ts.FocusedSessionID
				if focusSess == "" || !knownSessions[focusSess] {
					focusSess = firstLeafSession(tree)
				}
				for termID, sessID := range ts.TermToSession {
					if sessID == focusSess {
						if p, ok := termToPane[termID]; ok {
							targetSplitPane = p
							break
						}
					}
				}
			}
			cwd := explicitCwd
			if cwd == "" {
				if sessID := ts.TermToSession[targetSplitPane.TerminalID]; sessID != "" {
					cwd = sessionCwd(sessID)
				}
			}
			if cwd == "" {
				cwd = resolveAnchorCwd(client, targetSplitPane, explicitCwd)
			}
			if splitDir != "right" && splitDir != "down" {
				splitDir = "right"
			}
			newSessID := newSessionID()
			newPane, err := openDrawerPaneWithRatio(client, tabID, targetSplitPane.PaneID, splitDir, 0.5, newSessID, cwd, true)
			if err != nil {
				return err
			}
			ts.TermToSession[newPane.TerminalID] = newSessID
			ts.FocusedSessionID = newSessID
			if updatedLayout, err := client.ExportLayout(tabID); err == nil {
				if updatedSplit := findDrawerSplit(&updatedLayout.Root, anchorPaneID, paneToTerm, ts.TermToSession, true); updatedSplit != nil {
					paneByID[newPane.PaneID] = *newPane
					paneToTerm[newPane.PaneID] = newPane.TerminalID
					if updatedTree := layoutToDrawerTree(updatedSplit.Second, paneByID, ts.TermToSession); updatedTree != nil {
						ts.Tree = updatedTree
					}
				}
			}
			return nil

		default:
			return fmt.Errorf("unknown action: %s", action)
		}
	})
}

func main() {
	action := "toggle"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action = args[0]
		args = args[1:]
	}

	fs := flag.NewFlagSet("herdr-drawer", flag.ContinueOnError)
	var paneID string
	var cwd string
	var direction string
	var sessionID string
	var rows int
	var cols int
	var fromNvim bool
	fs.StringVar(&paneID, "pane", "", "Target pane ID")
	fs.StringVar(&cwd, "cwd", "", "Working directory for new drawer")
	fs.StringVar(&direction, "direction", "right", "Split direction (right or down)")
	fs.StringVar(&sessionID, "session", "", "Drawer PTY session ID")
	fs.IntVar(&rows, "rows", 24, "Initial PTY rows")
	fs.IntVar(&cols, "cols", 80, "Initial PTY cols")
	fs.BoolVar(&fromNvim, "from-nvim", false, "Invoked from Neovim")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-drawer: %v\n", err)
		os.Exit(2)
	}

	switch action {
	case "host":
		if err := runHost(sessionID, cwd, uint16(rows), uint16(cols)); err != nil {
			fmt.Fprintf(os.Stderr, "herdr-drawer host: %v\n", err)
			os.Exit(1)
		}
		return
	case "attach":
		if err := runAttach(sessionID, cwd); err != nil {
			fmt.Fprintf(os.Stderr, "herdr-drawer attach: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := runAction(action, paneID, cwd, direction); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-drawer: %v\n", err)
		os.Exit(1)
	}
}

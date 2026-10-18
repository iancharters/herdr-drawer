package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	stashWorkspaceLabel = "__drawer_stash__"
	stashTabLabel       = "drawer"
	defaultDrawerRatio  = 0.70
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

type WorkspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
	ActiveTabID string `json:"active_tab_id"`
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
	Type       string      `json:"type"` // "leaf" or "split"
	TerminalID string      `json:"terminal_id,omitempty"`
	Direction  string      `json:"direction,omitempty"`
	Ratio      float64     `json:"ratio,omitempty"`
	First      *DrawerNode `json:"first,omitempty"`
	Second     *DrawerNode `json:"second,omitempty"`
}

type TabDrawerState struct {
	TabID             string          `json:"tab_id"`
	WorkspaceID       string          `json:"workspace_id"`
	AnchorTerminalID  string          `json:"anchor_terminal_id"`
	DrawerRatio       float64         `json:"drawer_ratio"`
	Visible           bool            `json:"visible"`
	FocusedTerminalID string          `json:"focused_terminal_id"`
	KnownTerminals    map[string]bool `json:"known_terminals"`
	Tree              *DrawerNode     `json:"tree,omitempty"`
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

func (c *RPCClient) ListWorkspaces() ([]WorkspaceInfo, error) {
	var res struct {
		Workspaces []WorkspaceInfo `json:"workspaces"`
	}
	if err := c.Call("workspace.list", map[string]interface{}{}, &res); err != nil {
		return nil, err
	}
	return res.Workspaces, nil
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

func (c *RPCClient) SplitPane(targetPaneID, direction string, ratio float64, cwd string, focus bool) (*PaneInfo, error) {
	params := map[string]interface{}{
		"target_pane_id": targetPaneID,
		"direction":      direction,
		"ratio":          ratio,
		"focus":          focus,
	}
	if cwd != "" {
		params["cwd"] = cwd
	}
	var res struct {
		Pane PaneInfo `json:"pane"`
	}
	if err := c.Call("pane.split", params, &res); err != nil {
		return nil, err
	}
	return &res.Pane, nil
}

type MoveResult struct {
	Pane             PaneInfo       `json:"pane"`
	CreatedWorkspace *WorkspaceInfo `json:"created_workspace,omitempty"`
}

func (c *RPCClient) MovePaneToStash(paneID, stashWorkspaceID string) (*MoveResult, error) {
	var dest map[string]interface{}
	if stashWorkspaceID == "" {
		dest = map[string]interface{}{
			"type":      "new_workspace",
			"label":     stashWorkspaceLabel,
			"tab_label": stashTabLabel,
		}
	} else {
		dest = map[string]interface{}{
			"type":         "new_tab",
			"workspace_id": stashWorkspaceID,
			"label":        stashTabLabel,
		}
	}
	params := map[string]interface{}{
		"pane_id":     paneID,
		"focus":       false,
		"destination": dest,
	}
	var res struct {
		MoveResult MoveResult `json:"move_result"`
	}
	if err := c.Call("pane.move", params, &res); err != nil {
		return nil, err
	}
	return &res.MoveResult, nil
}

func (c *RPCClient) MovePaneToTabSplit(paneID, tabID, targetPaneID, splitDir string, ratio float64, focus bool) (*MoveResult, error) {
	dest := map[string]interface{}{
		"type":           "tab",
		"tab_id":         tabID,
		"target_pane_id": targetPaneID,
		"split":          splitDir,
		"ratio":          ratio,
	}
	params := map[string]interface{}{
		"pane_id":     paneID,
		"focus":       focus,
		"destination": dest,
	}
	var res struct {
		MoveResult MoveResult `json:"move_result"`
	}
	if err := c.Call("pane.move", params, &res); err != nil {
		return nil, err
	}
	return &res.MoveResult, nil
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

func layoutContainsAnyTerm(node *LayoutNode, paneToTerm map[string]string, known map[string]bool) bool {
	if node == nil || len(known) == 0 {
		return false
	}
	if node.Type == "pane" {
		termID := paneToTerm[node.PaneID]
		return termID != "" && known[termID]
	}
	return layoutContainsAnyTerm(node.First, paneToTerm, known) || layoutContainsAnyTerm(node.Second, paneToTerm, known)
}

// findDrawerSplit locates the Split(direction="down") node where First contains
// the anchor pane and Second is the drawer subtree.
func findDrawerSplit(root *LayoutNode, anchorPaneID string, paneToTerm map[string]string, known map[string]bool, allowFallback bool) *LayoutNode {
	if root == nil || root.Type != "split" {
		return nil
	}
	// Search deeper first so we find the tightest enclosing down-split for anchorPaneID.
	if found := findDrawerSplit(root.First, anchorPaneID, paneToTerm, known, allowFallback); found != nil {
		return found
	}
	if found := findDrawerSplit(root.Second, anchorPaneID, paneToTerm, known, allowFallback); found != nil {
		return found
	}
	if root.Direction == "down" && layoutContainsPane(root.First, anchorPaneID) {
		if layoutContainsAnyTerm(root.Second, paneToTerm, known) || allowFallback {
			return root
		}
	}
	return nil
}

func layoutToDrawerTree(node *LayoutNode, paneToTerm map[string]string) *DrawerNode {
	if node == nil {
		return nil
	}
	if node.Type == "pane" {
		termID := paneToTerm[node.PaneID]
		if termID == "" {
			return nil
		}
		return &DrawerNode{
			Type:       "leaf",
			TerminalID: termID,
		}
	}
	first := layoutToDrawerTree(node.First, paneToTerm)
	second := layoutToDrawerTree(node.Second, paneToTerm)
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

func collectDrawerTerminals(node *DrawerNode, out map[string]bool) {
	if node == nil {
		return
	}
	if node.Type == "leaf" {
		if node.TerminalID != "" {
			out[node.TerminalID] = true
		}
		return
	}
	collectDrawerTerminals(node.First, out)
	collectDrawerTerminals(node.Second, out)
}

func collectDrawerLeavesOrdered(node *DrawerNode) []string {
	if node == nil {
		return nil
	}
	if node.Type == "leaf" {
		if node.TerminalID != "" {
			return []string{node.TerminalID}
		}
		return nil
	}
	left := collectDrawerLeavesOrdered(node.First)
	right := collectDrawerLeavesOrdered(node.Second)
	return append(left, right...)
}

func pruneDrawerTree(node *DrawerNode, aliveTermToPane map[string]PaneInfo, excludeTermID string) *DrawerNode {
	if node == nil {
		return nil
	}
	if node.Type == "leaf" {
		if node.TerminalID == "" || node.TerminalID == excludeTermID {
			return nil
		}
		if _, ok := aliveTermToPane[node.TerminalID]; !ok {
			return nil
		}
		return &DrawerNode{
			Type:       "leaf",
			TerminalID: node.TerminalID,
		}
	}
	first := pruneDrawerTree(node.First, aliveTermToPane, excludeTermID)
	second := pruneDrawerTree(node.Second, aliveTermToPane, excludeTermID)
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

func firstLeafTerm(node *DrawerNode) string {
	if node == nil {
		return ""
	}
	if node.Type == "leaf" {
		return node.TerminalID
	}
	if t := firstLeafTerm(node.First); t != "" {
		return t
	}
	return firstLeafTerm(node.Second)
}

func reconstructSplitsPreOrder(client *RPCClient, tabID string, node *DrawerNode, termToPaneID map[string]string) error {
	if node == nil || node.Type != "split" {
		return nil
	}
	leftTerm := firstLeafTerm(node.First)
	rightTerm := firstLeafTerm(node.Second)
	targetPaneID := termToPaneID[leftTerm]
	movingPaneID := termToPaneID[rightTerm]
	if targetPaneID == "" || movingPaneID == "" {
		return fmt.Errorf("missing pane mapping for split reconstruction (left=%s right=%s)", leftTerm, rightTerm)
	}
	ratio := node.Ratio
	if ratio <= 0.05 || ratio >= 0.95 {
		ratio = 0.5
	}
	dir := node.Direction
	if dir != "right" && dir != "down" {
		dir = "right"
	}
	res, err := client.MovePaneToTabSplit(movingPaneID, tabID, targetPaneID, dir, ratio, false)
	if err != nil {
		return err
	}
	termToPaneID[rightTerm] = res.Pane.PaneID

	if err := reconstructSplitsPreOrder(client, tabID, node.First, termToPaneID); err != nil {
		return err
	}
	return reconstructSplitsPreOrder(client, tabID, node.Second, termToPaneID)
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

func findStashWorkspaceID(workspaces []WorkspaceInfo) string {
	for _, ws := range workspaces {
		if ws.Label == stashWorkspaceLabel {
			return ws.WorkspaceID
		}
	}
	return ""
}

func hideDrawer(client *RPCClient, ts *TabDrawerState, layout *LayoutExport, drawerSplit *LayoutNode, currentPane *PaneInfo, paneByID map[string]PaneInfo, paneToTerm map[string]string, termToPane map[string]PaneInfo) error {
	tree := layoutToDrawerTree(drawerSplit.Second, paneToTerm)
	if tree == nil {
		return errors.New("drawer split had no valid panes")
	}

	known := map[string]bool{}
	collectDrawerTerminals(tree, known)

	ts.DrawerRatio = drawerSplit.Ratio
	ts.Tree = tree
	ts.KnownTerminals = known
	ts.Visible = false

	// Record which drawer pane had focus so we can restore focus to it on unhide.
	if currentPane != nil && known[currentPane.TerminalID] {
		ts.FocusedTerminalID = currentPane.TerminalID
	} else if focusedTerm := paneToTerm[layout.FocusedPaneID]; known[focusedTerm] {
		ts.FocusedTerminalID = focusedTerm
	}
	if ts.FocusedTerminalID == "" || !known[ts.FocusedTerminalID] {
		ts.FocusedTerminalID = firstLeafTerm(tree)
	}

	workspaces, err := client.ListWorkspaces()
	if err != nil {
		return err
	}
	stashWsID := findStashWorkspaceID(workspaces)

	leaves := collectDrawerLeavesOrdered(tree)
	for _, termID := range leaves {
		p, ok := termToPane[termID]
		if !ok {
			continue
		}
		res, err := client.MovePaneToStash(p.PaneID, stashWsID)
		if err != nil {
			return err
		}
		if stashWsID == "" && res.CreatedWorkspace != nil {
			stashWsID = res.CreatedWorkspace.WorkspaceID
		}
	}

	// Focus the anchor pane if it exists in this tab.
	var focusPaneID string
	if anchorPane, ok := termToPane[ts.AnchorTerminalID]; ok && anchorPane.TabID == ts.TabID {
		focusPaneID = anchorPane.PaneID
	} else if drawerSplit.First != nil {
		firstAnchorTerm := firstLeafTerm(layoutToDrawerTree(drawerSplit.First, paneToTerm))
		if p, ok := termToPane[firstAnchorTerm]; ok {
			focusPaneID = p.PaneID
		}
	}
	if focusPaneID != "" {
		_ = client.FocusPane(focusPaneID)
	}

	return nil
}

func showDrawer(client *RPCClient, ts *TabDrawerState, currentPane *PaneInfo, explicitCwd string, termToPane map[string]PaneInfo) error {
	// Resolve the anchor pane in the current tab.
	anchorPane := *currentPane
	if ts.AnchorTerminalID != "" {
		if p, ok := termToPane[ts.AnchorTerminalID]; ok && p.TabID == currentPane.TabID {
			anchorPane = p
		}
	}
	ts.AnchorTerminalID = anchorPane.TerminalID
	ts.TabID = anchorPane.TabID
	ts.WorkspaceID = anchorPane.WorkspaceID
	if ts.DrawerRatio <= 0.1 || ts.DrawerRatio >= 0.9 {
		ts.DrawerRatio = defaultDrawerRatio
	}
	if ts.KnownTerminals == nil {
		ts.KnownTerminals = map[string]bool{}
	}

	prunedTree := pruneDrawerTree(ts.Tree, termToPane, ts.AnchorTerminalID)
	if prunedTree == nil {
		// Create a brand-new drawer pane beneath the anchor pane.
		cwd := resolveAnchorCwd(client, anchorPane, explicitCwd)
		newPane, err := client.SplitPane(anchorPane.PaneID, "down", ts.DrawerRatio, cwd, true)
		if err != nil {
			return err
		}
		ts.Tree = &DrawerNode{
			Type:       "leaf",
			TerminalID: newPane.TerminalID,
		}
		ts.KnownTerminals = map[string]bool{newPane.TerminalID: true}
		ts.FocusedTerminalID = newPane.TerminalID
		ts.Visible = true
		return nil
	}

	// Reconstruct the stashed multi-pane split tree in pre-order.
	termToPaneID := make(map[string]string, len(termToPane))
	for termID, p := range termToPane {
		termToPaneID[termID] = p.PaneID
	}

	rootLeafTerm := firstLeafTerm(prunedTree)
	rootLeafPaneID := termToPaneID[rootLeafTerm]
	if rootLeafPaneID == "" {
		return fmt.Errorf("root drawer terminal %s not found", rootLeafTerm)
	}

	moveRes, err := client.MovePaneToTabSplit(rootLeafPaneID, ts.TabID, anchorPane.PaneID, "down", ts.DrawerRatio, false)
	if err != nil {
		return err
	}
	termToPaneID[rootLeafTerm] = moveRes.Pane.PaneID

	if err := reconstructSplitsPreOrder(client, ts.TabID, prunedTree, termToPaneID); err != nil {
		return err
	}

	known := map[string]bool{}
	collectDrawerTerminals(prunedTree, known)
	ts.Tree = prunedTree
	ts.KnownTerminals = known
	ts.Visible = true

	focusTerm := ts.FocusedTerminalID
	if focusTerm == "" || !known[focusTerm] {
		focusTerm = rootLeafTerm
		ts.FocusedTerminalID = focusTerm
	}
	if focusPaneID := termToPaneID[focusTerm]; focusPaneID != "" {
		_ = client.FocusPane(focusPaneID)
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

		currentPane, err := findTargetPane(panes, explicitPaneID)
		if err != nil {
			return err
		}

		tabID := currentPane.TabID
		ts, ok := sf.Tabs[tabID]
		if !ok {
			ts = &TabDrawerState{
				TabID:          tabID,
				WorkspaceID:    currentPane.WorkspaceID,
				DrawerRatio:    defaultDrawerRatio,
				KnownTerminals: map[string]bool{},
			}
			sf.Tabs[tabID] = ts
		}
		if ts.KnownTerminals == nil {
			ts.KnownTerminals = map[string]bool{}
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
			drawerSplit = findDrawerSplit(&layout.Root, anchorPaneID, paneToTerm, ts.KnownTerminals, ts.Visible)
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
				// Already visible; focus the active drawer pane.
				tree := layoutToDrawerTree(drawerSplit.Second, paneToTerm)
				known := map[string]bool{}
				collectDrawerTerminals(tree, known)
				targetTerm := ts.FocusedTerminalID
				if targetTerm == "" || !known[targetTerm] {
					targetTerm = firstLeafTerm(tree)
				}
				if p, ok := termToPane[targetTerm]; ok {
					return client.FocusPane(p.PaneID)
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
				if err := showDrawer(client, ts, currentPane, explicitCwd, termToPane); err != nil {
					return err
				}
				return nil
			}
			tree := layoutToDrawerTree(drawerSplit.Second, paneToTerm)
			known := map[string]bool{}
			collectDrawerTerminals(tree, known)

			targetSplitPane := *currentPane
			if !known[currentPane.TerminalID] {
				focusTerm := ts.FocusedTerminalID
				if focusTerm == "" || !known[focusTerm] {
					focusTerm = firstLeafTerm(tree)
				}
				if p, ok := termToPane[focusTerm]; ok {
					targetSplitPane = p
				}
			}
			cwd := resolveAnchorCwd(client, targetSplitPane, explicitCwd)
			if splitDir != "right" && splitDir != "down" {
				splitDir = "right"
			}
			newPane, err := client.SplitPane(targetSplitPane.PaneID, splitDir, 0.5, cwd, true)
			if err != nil {
				return err
			}
			ts.KnownTerminals[newPane.TerminalID] = true
			ts.FocusedTerminalID = newPane.TerminalID
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
	var fromNvim bool
	fs.StringVar(&paneID, "pane", "", "Target pane ID")
	fs.StringVar(&cwd, "cwd", "", "Working directory for new drawer")
	fs.StringVar(&direction, "direction", "right", "Split direction (right or down)")
	fs.BoolVar(&fromNvim, "from-nvim", false, "Invoked from Neovim")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-drawer: %v\n", err)
		os.Exit(2)
	}

	if err := runAction(action, paneID, cwd, direction); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-drawer: %v\n", err)
		os.Exit(1)
	}
}

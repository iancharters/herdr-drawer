package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	defaultUtilitySize = 0.30
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
	Label         string `json:"label"`
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
	ProfileID        string            `json:"profile_id,omitempty"`
	TabID            string            `json:"tab_id"`
	WorkspaceID      string            `json:"workspace_id"`
	AnchorTerminalID string            `json:"anchor_terminal_id"`
	UtilityRatio     float64           `json:"utility_ratio,omitempty"`
	DrawerRatio      float64           `json:"drawer_ratio,omitempty"` // legacy: anchor ratio
	Visible          bool              `json:"visible"`
	FocusedSessionID string            `json:"focused_session_id"`
	TermToSession    map[string]string `json:"term_to_session"`
	Tree             *DrawerNode       `json:"tree,omitempty"`
}

type StateFile struct {
	Tabs map[string]*TabDrawerState `json:"tabs"`
}

type placementSpec struct {
	layoutDirection string
	utilityOnFirst  bool
}

func stateKey(tabID, profileID string) string {
	if strings.TrimSpace(profileID) == "" {
		profileID = defaultProfileName
	}
	return tabID + "::" + profileID
}

func normalizeUtilityRatio(ts *TabDrawerState) {
	if ts == nil {
		return
	}
	if ts.UtilityRatio >= 0.1 && ts.UtilityRatio <= 0.9 {
		return
	}
	legacy := ts.DrawerRatio
	if legacy >= 0.1 && legacy <= 0.9 {
		u := 1.0 - legacy
		if u >= 0.1 && u <= 0.9 {
			ts.UtilityRatio = u
			return
		}
	}
	ts.UtilityRatio = defaultUtilitySize
}

func normalizeStateFile(sf *StateFile) {
	if sf == nil {
		return
	}
	if sf.Tabs == nil {
		sf.Tabs = map[string]*TabDrawerState{}
	}
	normalized := make(map[string]*TabDrawerState, len(sf.Tabs))
	for key, ts := range sf.Tabs {
		if ts == nil {
			continue
		}
		if ts.ProfileID == "" {
			ts.ProfileID = defaultProfileName
		}
		if ts.TabID == "" {
			if parts := strings.SplitN(key, "::", 2); len(parts) > 0 {
				ts.TabID = parts[0]
			} else {
				ts.TabID = key
			}
		}
		normalizeUtilityRatio(ts)
		normalized[stateKey(ts.TabID, ts.ProfileID)] = ts
	}
	sf.Tabs = normalized
}

func normalizePlacement(dir string) placementSpec {
	switch strings.ToLower(strings.TrimSpace(dir)) {
	case "up":
		return placementSpec{layoutDirection: "down", utilityOnFirst: true}
	case "left":
		return placementSpec{layoutDirection: "right", utilityOnFirst: true}
	case "right":
		return placementSpec{layoutDirection: "right", utilityOnFirst: false}
	default:
		return placementSpec{layoutDirection: "down", utilityOnFirst: false}
	}
}

func splitRatioForPlacement(size float64, placement string) float64 {
	if size < 0.1 || size > 0.9 {
		size = defaultUtilitySize
	}
	spec := normalizePlacement(placement)
	if spec.utilityOnFirst {
		return size
	}
	return 1.0 - size
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

func (c *RPCClient) GetPane(paneID string) (*PaneInfo, error) {
	var res struct {
		Pane PaneInfo `json:"pane"`
	}
	if err := c.Call("pane.get", map[string]interface{}{"pane_id": paneID}, &res); err != nil {
		return nil, err
	}
	return &res.Pane, nil
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

func (c *RPCClient) ApplyLayout(tabID string, root LayoutNode, focus bool) (*LayoutExport, error) {
	params := map[string]interface{}{
		"tab_id": tabID,
		"root":   root,
		"focus":  focus,
	}
	var res struct {
		Layout LayoutExport `json:"layout"`
	}
	if err := c.Call("layout.apply", params, &res); err != nil {
		return nil, err
	}
	return &res.Layout, nil
}

func (c *RPCClient) MovePaneToTabSplit(paneID, tabID, direction, targetPaneID string, focus bool) error {
	dest := map[string]interface{}{
		"type":   "tab",
		"tab_id": tabID,
		"split":  direction,
	}
	if targetPaneID != "" {
		dest["target_pane_id"] = targetPaneID
	}
	params := map[string]interface{}{
		"pane_id":     paneID,
		"destination": dest,
		"focus":       focus,
	}
	return c.Call("pane.move", params, nil)
}

func (c *RPCClient) ClosePane(paneID string) error {
	return c.Call("pane.close", map[string]interface{}{"pane_id": paneID}, nil)
}

func (c *RPCClient) FocusPane(paneID string) error {
	return c.Call("pane.focus", map[string]interface{}{"pane_id": paneID}, nil)
}

func (c *RPCClient) SendText(paneID, text string) error {
	return c.Call("pane.send_text", map[string]interface{}{
		"pane_id": paneID,
		"text":    text,
	}, nil)
}

func (c *RPCClient) SwapPanes(sourcePaneID, targetPaneID string) error {
	return c.Call("pane.swap", map[string]interface{}{
		"source_pane_id": sourcePaneID,
		"target_pane_id": targetPaneID,
	}, nil)
}

func baseStateDir() string {
	if d := os.Getenv("HERDR_DRAWER_STATE_DIR"); d != "" {
		return d
	}
	return fmt.Sprintf("/tmp/herdr-drawer-%d", os.Getuid())
}

func stateDir() string {
	base := baseStateDir()
	if os.Getenv("HERDR_DRAWER_STATE_DIR") != "" {
		return base
	}
	sock := os.Getenv("HERDR_SOCKET_PATH")
	if sock == "" || strings.HasSuffix(sock, "/herdr/herdr.sock") {
		return base
	}
	var h uint64 = 14695981039346656037
	for i := 0; i < len(sock); i++ {
		h ^= uint64(sock[i])
		h *= 1099511628211
	}
	return filepath.Join(base, fmt.Sprintf("srv-%016x", h))
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
	}
	normalizeStateFile(sf)

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

func readStateSnapshot() (*StateFile, error) {
	dir := stateDir()
	lockFile, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_SH); err != nil {
		return nil, err
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	statePath := filepath.Join(dir, "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, err
	}
	sf := &StateFile{Tabs: map[string]*TabDrawerState{}}
	if err := json.Unmarshal(data, sf); err != nil {
		return nil, err
	}
	normalizeStateFile(sf)
	return sf, nil
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
	name := sanitizePaneID(paneID)
	for _, dir := range []string{stateDir(), baseStateDir()} {
		if data, err := os.ReadFile(filepath.Join(dir, "cwd", name)); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				return s
			}
		}
	}
	return ""
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

func cloneLayoutNode(node *LayoutNode) *LayoutNode {
	if node == nil {
		return nil
	}
	if node.Type == "pane" {
		// Keep pane references minimal when re-applying layouts. Carrying `cwd`
		// from exported layouts can be interpreted as "spawn new terminal pane".
		return &LayoutNode{
			Type:   "pane",
			PaneID: node.PaneID,
		}
	}
	return &LayoutNode{
		Type:      "split",
		Direction: node.Direction,
		Ratio:     node.Ratio,
		First:     cloneLayoutNode(node.First),
		Second:    cloneLayoutNode(node.Second),
	}
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

func splitBranch(split *LayoutNode, utilityOnFirst bool) (utility *LayoutNode, anchor *LayoutNode) {
	if split == nil {
		return nil, nil
	}
	if utilityOnFirst {
		return split.First, split.Second
	}
	return split.Second, split.First
}

func findDrawerSplit(root *LayoutNode, anchorPaneID string, paneToTerm map[string]string, termToSession map[string]string, placement, scope string, allowFallback bool) *LayoutNode {
	if root == nil || root.Type != "split" {
		return nil
	}
	spec := normalizePlacement(placement)
	if root.Direction == spec.layoutDirection {
		utilityBranch, anchorBranch := splitBranch(root, spec.utilityOnFirst)
		anchorOK := true
		if strings.ToLower(strings.TrimSpace(scope)) == "tab" {
			anchorOK = anchorPaneID == "" || layoutContainsPane(anchorBranch, anchorPaneID)
		} else {
			anchorOK = layoutContainsPane(anchorBranch, anchorPaneID)
		}
		if anchorOK && (layoutContainsAnyTerm(utilityBranch, paneToTerm, termToSession) || allowFallback) {
			return root
		}
	}
	if found := findDrawerSplit(root.First, anchorPaneID, paneToTerm, termToSession, placement, scope, allowFallback); found != nil {
		return found
	}
	if found := findDrawerSplit(root.Second, anchorPaneID, paneToTerm, termToSession, placement, scope, allowFallback); found != nil {
		return found
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

func findSiblingSessionID(node *LayoutNode, targetPaneID string, paneToTerm map[string]string, termToSession map[string]string) string {
	if node == nil || node.Type != "split" {
		return ""
	}
	if s := findSiblingSessionID(node.First, targetPaneID, paneToTerm, termToSession); s != "" {
		return s
	}
	if s := findSiblingSessionID(node.Second, targetPaneID, paneToTerm, termToSession); s != "" {
		return s
	}
	if layoutContainsPane(node.Second, targetPaneID) {
		if sibPaneID := firstLayoutPaneID(node.First); sibPaneID != "" {
			return termToSession[paneToTerm[sibPaneID]]
		}
	}
	if layoutContainsPane(node.First, targetPaneID) {
		if sibPaneID := firstLayoutPaneID(node.Second); sibPaneID != "" {
			return termToSession[paneToTerm[sibPaneID]]
		}
	}
	return ""
}

func ensureDrawerSubtreeSessions(node *LayoutNode, paneByID map[string]PaneInfo, termToSession map[string]string) {
	for _, paneID := range collectLayoutPaneIDs(node) {
		p, ok := paneByID[paneID]
		if !ok || p.TerminalID == "" || termToSession[p.TerminalID] != "" {
			continue
		}
		cwd := p.ForegroundCwd
		if cwd == "" {
			cwd = p.Cwd
		}
		newSessID := newSessionID()
		_ = ensureSessionHost(newSessID, cwd, 24, 80)
		termToSession[p.TerminalID] = newSessID
	}
}

func adoptNativeDrawerSplits(tabID string) error {
	if tabID == "" {
		return nil
	}
	sock, err := resolveSocketPath()
	if err != nil {
		return err
	}
	client := &RPCClient{socketPath: sock}

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

		layout, err := client.ExportLayout(tabID)
		if err != nil {
			return err
		}
		for _, ts := range sf.Tabs {
			if ts == nil || ts.TabID != tabID || !ts.Visible || ts.AnchorTerminalID == "" {
				continue
			}
			if ts.TermToSession == nil {
				ts.TermToSession = map[string]string{}
			}
			anchorPane, ok := termToPane[ts.AnchorTerminalID]
			if !ok || anchorPane.TabID != tabID {
				continue
			}
			placement := "down"
			scope := "anchor"
			if ts.ProfileID != "" {
				if cfg, err := loadProfilesFile(); err == nil {
					p := resolveProfile(ts.ProfileID, cfg)
					placement = p.Placement
					scope = p.Scope
				}
			}

			drawerSplit := findDrawerSplit(&layout.Root, anchorPane.PaneID, paneToTerm, ts.TermToSession, placement, scope, false)
			if drawerSplit == nil {
				continue
			}
			utilitySubtree, _ := splitBranch(drawerSplit, normalizePlacement(placement).utilityOnFirst)
			drawerPaneIDs := collectLayoutPaneIDs(utilitySubtree)
			adoptedAny := false
			for _, paneID := range drawerPaneIDs {
				p, ok := paneByID[paneID]
				if !ok || p.TerminalID == "" || ts.TermToSession[p.TerminalID] != "" {
					continue
				}

				cwd := ""
				if sibSess := findSiblingSessionID(utilitySubtree, p.PaneID, paneToTerm, ts.TermToSession); sibSess != "" {
					cwd = sessionCwd(sibSess)
				}
				if cwd == "" && ts.FocusedSessionID != "" {
					cwd = sessionCwd(ts.FocusedSessionID)
				}
				if cwd == "" {
					cwd = p.ForegroundCwd
				}
				if cwd == "" {
					cwd = p.Cwd
				}

				newSessID := newSessionID()
				_ = ensureSessionHost(newSessID, cwd, 24, 80)
				newPane, err := openDrawerPaneWithRatio(client, tabID, p.PaneID, "right", 0.5, newSessID, cwd, "", p.Focused)
				if err != nil {
					killSession(newSessID)
					continue
				}
				_ = client.ClosePane(p.PaneID)
				if p.Focused {
					_ = client.FocusPane(newPane.PaneID)
					ts.FocusedSessionID = newSessID
				}

				ts.TermToSession[newPane.TerminalID] = newSessID
				paneByID[newPane.PaneID] = *newPane
				paneToTerm[newPane.PaneID] = newPane.TerminalID
				delete(paneByID, p.PaneID)
				delete(paneToTerm, p.PaneID)
				adoptedAny = true
			}

			if adoptedAny {
				if updatedLayout, err := client.ExportLayout(tabID); err == nil {
					if updatedSplit := findDrawerSplit(&updatedLayout.Root, anchorPane.PaneID, paneToTerm, ts.TermToSession, placement, scope, false); updatedSplit != nil {
						updatedSubtree, _ := splitBranch(updatedSplit, normalizePlacement(placement).utilityOnFirst)
						if updatedTree := layoutToDrawerTree(updatedSubtree, paneByID, ts.TermToSession); updatedTree != nil {
							ts.Tree = updatedTree
						}
					}
				}
			}
		}
		return nil
	})
}

func applyTabScopedRootSplit(client *RPCClient, tabID, placement string, utilityRatio float64, utilityPaneID string) error {
	if utilityPaneID == "" {
		return nil
	}
	spec := normalizePlacement(placement)
	if err := client.MovePaneToTabSplit(utilityPaneID, tabID, spec.layoutDirection, "", true); err != nil {
		return err
	}
	if spec.utilityOnFirst {
		opposite := "left"
		if spec.layoutDirection == "down" {
			opposite = "up"
		}
		_ = client.Call("pane.swap", map[string]interface{}{
			"source_pane_id": utilityPaneID,
			"direction":      opposite,
		}, nil)
	}
	return client.SetSplitRatio(tabID, []bool{}, splitRatioForPlacement(utilityRatio, placement))
}

func openDrawerPaneWithRatio(client *RPCClient, tabID, targetPaneID, placement string, ratio float64, sessionID, cwd, command string, focus bool) (*PaneInfo, error) {
	_ = ensureSessionHost(sessionID, cwd, 24, 80)
	spec := normalizePlacement(placement)
	pane, err := client.OpenPluginSplitPane(targetPaneID, spec.layoutDirection, cwd, sessionID, focus)
	if err != nil {
		return nil, err
	}
	if spec.utilityOnFirst {
		_ = client.SwapPanes(targetPaneID, pane.PaneID)
	}
	if ratio >= 0.1 && ratio <= 0.9 && math.Abs(ratio-0.5) > 0.005 {
		if layout, err := client.ExportLayout(tabID); err == nil {
			if path, ok := findSplitPath(&layout.Root, targetPaneID, pane.PaneID, nil); ok {
				_ = client.SetSplitRatio(tabID, path, ratio)
			}
		}
	}
	if focus {
		if updated, err := client.GetPane(pane.PaneID); err == nil && updated != nil {
			return updated, nil
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
	newPane, err := openDrawerPaneWithRatio(client, tabID, targetPane.PaneID, dir, ratio, rightLeaf.SessionID, rightLeaf.Cwd, "", false)
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
	referencedSessions := map[string]bool{}
	for stateID, ts := range sf.Tabs {
		if ts == nil {
			delete(sf.Tabs, stateID)
			continue
		}
		tabID := ts.TabID
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
			delete(sf.Tabs, stateID)
			continue
		}

		if ts.Visible {
			for termID, sessID := range ts.TermToSession {
				if p, ok := termToPane[termID]; !ok || p.TabID != tabID {
					if sessID != "" {
						killSession(sessID)
					}
					delete(ts.TermToSession, termID)
				}
			}
		}

		collectDrawerSessions(ts.Tree, referencedSessions)
		for _, sessID := range ts.TermToSession {
			if sessID != "" {
				referencedSessions[sessID] = true
			}
		}
	}

	for _, sessID := range listDaemonSessions() {
		if !referencedSessions[sessID] {
			killSession(sessID)
		}
	}
}

func hideDrawer(client *RPCClient, ts *TabDrawerState, profile EffectiveProfile, layout *LayoutExport, drawerSplit *LayoutNode, currentPane *PaneInfo, paneByID map[string]PaneInfo, paneToTerm map[string]string, termToPane map[string]PaneInfo) error {
	utilitySubtree, anchorSubtree := splitBranch(drawerSplit, normalizePlacement(profile.Placement).utilityOnFirst)
	ensureDrawerSubtreeSessions(utilitySubtree, paneByID, ts.TermToSession)
	tree := layoutToDrawerTree(utilitySubtree, paneByID, ts.TermToSession)
	if tree == nil {
		return errors.New("drawer split had no valid panes")
	}

	knownSessions := map[string]bool{}
	collectDrawerSessions(tree, knownSessions)

	prevSessions := map[string]bool{}
	collectDrawerSessions(ts.Tree, prevSessions)
	for _, sessID := range ts.TermToSession {
		if sessID != "" {
			prevSessions[sessID] = true
		}
	}
	for sessID := range prevSessions {
		if !knownSessions[sessID] {
			killSession(sessID)
		}
	}

	ts.UtilityRatio = profile.SizeRatio
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
	} else if anchorSubtree != nil {
		focusPaneID = firstLayoutPaneID(anchorSubtree)
	}
	if focusPaneID != "" {
		_ = client.FocusPane(focusPaneID)
	}

	drawerPaneIDs := collectLayoutPaneIDs(utilitySubtree)
	for _, paneID := range drawerPaneIDs {
		_ = client.ClosePane(paneID)
	}

	if !profile.PersistOnHide {
		for sessID := range knownSessions {
			killSession(sessID)
		}
		ts.Tree = nil
		ts.FocusedSessionID = ""
	}
	ts.TermToSession = map[string]string{}

	return nil
}

func showDrawer(client *RPCClient, ts *TabDrawerState, profile EffectiveProfile, currentPane *PaneInfo, explicitCwd string, termToPane map[string]PaneInfo) error {
	anchorPane := *currentPane
	if ts.AnchorTerminalID != "" {
		if p, ok := termToPane[ts.AnchorTerminalID]; ok && p.TabID == currentPane.TabID {
			anchorPane = p
		}
	}
	ts.AnchorTerminalID = anchorPane.TerminalID
	ts.TabID = anchorPane.TabID
	ts.WorkspaceID = anchorPane.WorkspaceID
	ts.UtilityRatio = profile.SizeRatio
	ts.TermToSession = map[string]string{}

	prunedTree := ts.Tree
	if profile.PersistOnHide {
		prunedTree = pruneDrawerTree(ts.Tree, isSessionAlive)
	} else {
		prunedTree = nil
	}
	if prunedTree == nil {
		cwd := resolveAnchorCwd(client, anchorPane, explicitCwd)
		sessionID := newSessionID()
		command := renderProfileCommand(profile.Command, cwd)
		newPane, err := openDrawerPaneWithRatio(client, anchorPane.TabID, anchorPane.PaneID, profile.Placement, splitRatioForPlacement(profile.SizeRatio, profile.Placement), sessionID, cwd, command, true)
		if err != nil {
			return err
		}
		if profile.Scope == "tab" {
			if err := applyTabScopedRootSplit(client, ts.TabID, profile.Placement, profile.SizeRatio, newPane.PaneID); err != nil {
				return err
			}
			if updated, err := client.GetPane(newPane.PaneID); err == nil && updated != nil {
				newPane = updated
			}
		}
		if command != "" {
			if err := sendTextWithRetry(client, newPane.PaneID, command+"\n"); err != nil {
				return fmt.Errorf("send profile command: %w", err)
			}
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

	rootPane, err := openDrawerPaneWithRatio(client, ts.TabID, anchorPane.PaneID, profile.Placement, splitRatioForPlacement(profile.SizeRatio, profile.Placement), rootLeaf.SessionID, rootLeaf.Cwd, "", false)
	if err != nil {
		return err
	}
	if profile.Scope == "tab" {
		if err := applyTabScopedRootSplit(client, ts.TabID, profile.Placement, profile.SizeRatio, rootPane.PaneID); err != nil {
			return err
		}
		if updated, err := client.GetPane(rootPane.PaneID); err == nil && updated != nil {
			rootPane = updated
		}
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

func sendTextWithRetry(client *RPCClient, paneID, text string) error {
	var lastErr error
	for i := 0; i < 20; i++ {
		if err := client.SendText(paneID, text); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return lastErr
}

func cleanupSessions(client *RPCClient, sf *StateFile, termToPane map[string]PaneInfo, workspaceID string, all bool) {
	targetTabs := map[string]bool{}
	if !all && workspaceID != "" {
		for _, p := range termToPane {
			if p.WorkspaceID == workspaceID {
				targetTabs[p.TabID] = true
			}
		}
	}

	for stateID, ts := range sf.Tabs {
		if ts == nil {
			delete(sf.Tabs, stateID)
			continue
		}
		matches := all
		if !matches {
			if workspaceID != "" {
				matches = ts.WorkspaceID == workspaceID || targetTabs[ts.TabID]
			}
		}
		if !matches {
			continue
		}

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
		delete(sf.Tabs, stateID)
	}

	if all {
		for _, sessID := range listDaemonSessions() {
			killSession(sessID)
		}
	}
}

func runAction(action, explicitPaneID, explicitCwd, splitDir, profileName, cleanupScope string) error {
	sock, err := resolveSocketPath()
	if err != nil {
		return err
	}
	client := &RPCClient{socketPath: sock}
	profiles, err := loadProfilesFile()
	if err != nil {
		return fmt.Errorf("load profiles: %w", err)
	}
	profileName = profileNameFromFlagOrDefault(profileName, profiles)
	profile := resolveProfile(profileName, profiles)

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

		if action == "cleanup" {
			scope := strings.ToLower(strings.TrimSpace(cleanupScope))
			if scope == "" {
				scope = "workspace"
			}
			if scope == "all" {
				cleanupSessions(client, sf, termToPane, "", true)
				return nil
			}
			currentPane, err := findTargetPane(panes, explicitPaneID)
			if err != nil {
				return err
			}
			cleanupSessions(client, sf, termToPane, currentPane.WorkspaceID, false)
			return nil
		}

		gcOrphanedDrawers(client, sf, panes, termToPane)

		currentPane, err := findTargetPane(panes, explicitPaneID)
		if err != nil {
			return err
		}

		tabID := currentPane.TabID
		key := stateKey(tabID, profile.Name)
		ts, ok := sf.Tabs[key]
		if !ok {
			ts = &TabDrawerState{
				ProfileID:     profile.Name,
				TabID:         tabID,
				WorkspaceID:   currentPane.WorkspaceID,
				UtilityRatio:  profile.SizeRatio,
				TermToSession: map[string]string{},
			}
			sf.Tabs[key] = ts
		}
		if ts.TermToSession == nil {
			ts.TermToSession = map[string]string{}
		}
		ts.ProfileID = profile.Name

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
			drawerSplit = findDrawerSplit(&layout.Root, anchorPaneID, paneToTerm, ts.TermToSession, profile.Placement, profile.Scope, false)
		}
		isCurrentlyVisible := drawerSplit != nil
		utilitySubtree, _ := splitBranch(drawerSplit, normalizePlacement(profile.Placement).utilityOnFirst)

		switch action {
		case "toggle":
			if isCurrentlyVisible {
				return hideDrawer(client, ts, profile, layout, drawerSplit, currentPane, paneByID, paneToTerm, termToPane)
			}
			ts.Visible = false
			return showDrawer(client, ts, profile, currentPane, explicitCwd, termToPane)

		case "open":
			if isCurrentlyVisible {
				tree := layoutToDrawerTree(utilitySubtree, paneByID, ts.TermToSession)
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
			return showDrawer(client, ts, profile, currentPane, explicitCwd, termToPane)

		case "close":
			if !isCurrentlyVisible {
				ts.Visible = false
				return nil
			}
			return hideDrawer(client, ts, profile, layout, drawerSplit, currentPane, paneByID, paneToTerm, termToPane)

		case "split":
			if !isCurrentlyVisible {
				return showDrawer(client, ts, profile, currentPane, explicitCwd, termToPane)
			}
			tree := layoutToDrawerTree(utilitySubtree, paneByID, ts.TermToSession)
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
			newPane, err := openDrawerPaneWithRatio(client, tabID, targetSplitPane.PaneID, splitDir, 0.5, newSessID, cwd, "", true)
			if err != nil {
				return err
			}
			ts.TermToSession[newPane.TerminalID] = newSessID
			ts.FocusedSessionID = newSessID
			if updatedLayout, err := client.ExportLayout(tabID); err == nil {
				if updatedSplit := findDrawerSplit(&updatedLayout.Root, anchorPaneID, paneToTerm, ts.TermToSession, profile.Placement, profile.Scope, true); updatedSplit != nil {
					paneByID[newPane.PaneID] = *newPane
					paneToTerm[newPane.PaneID] = newPane.TerminalID
					updatedSubtree, _ := splitBranch(updatedSplit, normalizePlacement(profile.Placement).utilityOnFirst)
					if updatedTree := layoutToDrawerTree(updatedSubtree, paneByID, ts.TermToSession); updatedTree != nil {
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

const nvimPluginLoaderLua = `local function find_herdr_drawer()
  local config_dir = vim.env.XDG_CONFIG_HOME
  if not config_dir or config_dir == "" then
    config_dir = vim.fn.expand("~/.config")
  end
  local plugins_file = config_dir .. "/herdr/plugins.json"
  local uv = vim.uv or vim.loop
  if uv and uv.fs_stat(plugins_file) then
    local ok, lines = pcall(vim.fn.readfile, plugins_file)
    if ok and lines then
      local decoded_ok, plugins = pcall(vim.json.decode, table.concat(lines, "\n"))
      if decoded_ok and type(plugins) == "table" then
        for _, plugin in ipairs(plugins) do
          if
            type(plugin) == "table"
            and plugin.plugin_root
            and (
              plugin.plugin_id == "herdr-drawer"
              or (type(plugin.plugin_id) == "string" and plugin.plugin_id:match("%.herdr%-drawer$"))
            )
          then
            local script = plugin.plugin_root .. "/editor/nvim.lua"
            if uv.fs_stat(script) then
              return script
            end
          end
        end
      end
    end
  end
end

local script = find_herdr_drawer()
if script then
  dofile(script)
end

return {}
`

func defaultNvimPluginPath() string {
	configDir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configDir == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			configDir = filepath.Join(home, ".config")
		} else {
			configDir = ".config"
		}
	}
	return filepath.Join(configDir, "nvim", "lua", "plugins", "herdr-drawer.lua")
}

func displayHomePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+string(os.PathSeparator)) {
			return "~" + p[len(home):]
		}
	}
	return p
}

func expandNvimConfigPath(input, defaultPath string) string {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return defaultPath
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if raw == "~" {
			raw = home
		} else if strings.HasPrefix(raw, "~/") {
			raw = filepath.Join(home, raw[2:])
		}
	}
	if strings.HasSuffix(raw, "/") || strings.HasSuffix(raw, string(os.PathSeparator)) {
		return filepath.Join(raw, "herdr-drawer.lua")
	}
	if st, err := os.Stat(raw); err == nil && st.IsDir() {
		return filepath.Join(raw, "herdr-drawer.lua")
	}
	return filepath.Clean(raw)
}

func isStdinTTY() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (st.Mode() & os.ModeCharDevice) != 0
}

func runSetup(explicitPath string, popup bool, in io.Reader, out io.Writer) error {
	if strings.TrimSpace(explicitPath) == "" && !popup && !isStdinTTY() {
		sock, err := resolveSocketPath()
		if err != nil {
			return err
		}
		client := &RPCClient{socketPath: sock}
		return client.Call("plugin.pane.open", map[string]interface{}{
			"plugin_id":  pluginID,
			"entrypoint": "setup",
			"placement":  "popup",
			"focus":      true,
		}, nil)
	}

	defPath := defaultNvimPluginPath()
	reader := bufio.NewReader(in)
	targetPath := strings.TrimSpace(explicitPath)
	if targetPath == "" {
		fmt.Fprintf(out, "Install Neovim plugin config to [%s]\nPress Enter for default, or type a custom path: ", displayHomePath(defPath))
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		targetPath = expandNvimConfigPath(line, defPath)
	} else {
		targetPath = expandNvimConfigPath(targetPath, defPath)
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", filepath.Dir(targetPath), err)
	}
	if err := os.WriteFile(targetPath, []byte(nvimPluginLoaderLua), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", targetPath, err)
	}

	fmt.Fprintf(out, "\nInstalled Neovim plugin config to %s\nRestart Neovim for the changes to take effect.\n", displayHomePath(targetPath))
	if popup {
		fmt.Fprint(out, "\nPress Enter to close...")
		_, _ = reader.ReadString('\n')
	}
	return nil
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
	var scope string
	var sessionID string
	var profileName string
	var setupPath string
	var rows int
	var cols int
	var fromNvim bool
	var popup bool
	fs.StringVar(&paneID, "pane", "", "Target pane ID")
	fs.StringVar(&cwd, "cwd", "", "Working directory for new drawer")
	fs.StringVar(&direction, "direction", "right", "Split direction (right or down)")
	fs.StringVar(&scope, "scope", "workspace", "Scope for cleanup action (workspace or all)")
	fs.StringVar(&sessionID, "session", "", "Drawer PTY session ID")
	fs.StringVar(&profileName, "profile", "", "Profile name from profiles.toml")
	fs.StringVar(&setupPath, "path", "", "Target path for Neovim plugin config (used with setup)")
	fs.IntVar(&rows, "rows", 24, "Initial PTY rows")
	fs.IntVar(&cols, "cols", 80, "Initial PTY cols")
	fs.BoolVar(&fromNvim, "from-nvim", false, "Invoked from Neovim")
	fs.BoolVar(&popup, "popup", false, "Wait for Enter before closing setup popup")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-drawer: %v\n", err)
		os.Exit(2)
	}

	switch action {
	case "daemon", "host":
		if err := runDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "herdr-drawer daemon: %v\n", err)
			os.Exit(1)
		}
		return
	case "attach":
		if err := runAttach(sessionID, cwd); err != nil {
			fmt.Fprintf(os.Stderr, "herdr-drawer attach: %v\n", err)
			os.Exit(1)
		}
		return
	case "setup", "install-nvim":
		if err := runSetup(setupPath, popup, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "herdr-drawer setup: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := runAction(action, paneID, cwd, direction, profileName, scope); err != nil {
		fmt.Fprintf(os.Stderr, "herdr-drawer: %v\n", err)
		os.Exit(1)
	}
}

package main

import (
	"reflect"
	"testing"
)

func TestFindDrawerSplitAndSnapshot(t *testing.T) {
	// Tab layout:
	// Split(right, 0.75)
	//   First: Split(down, 0.70)
	//     First:  Pane(w4:p1 [term_nvim])
	//     Second: Split(right, 0.50)
	//       First:  Pane(w4:p5 [term_d1])
	//       Second: Pane(w4:p6 [term_d2])
	//   Second: Pane(w4:p4 [term_agent])
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

	paneToTerm := map[string]string{
		"w4:p1": "term_nvim",
		"w4:p4": "term_agent",
		"w4:p5": "term_d1",
		"w4:p6": "term_d2",
	}
	known := map[string]bool{
		"term_d1": true,
	}

	split := findDrawerSplit(root, "w4:p1", paneToTerm, known, false)
	if split == nil {
		t.Fatalf("expected to find drawer split")
	}
	if split.Ratio != 0.70 {
		t.Fatalf("expected drawer split ratio 0.70, got %v", split.Ratio)
	}

	tree := layoutToDrawerTree(split.Second, paneToTerm)
	if tree == nil || tree.Type != "split" || tree.Direction != "right" || tree.Ratio != 0.50 {
		t.Fatalf("unexpected drawer tree: %+v", tree)
	}
	leaves := collectDrawerLeavesOrdered(tree)
	expectedLeaves := []string{"term_d1", "term_d2"}
	if !reflect.DeepEqual(leaves, expectedLeaves) {
		t.Fatalf("expected leaves %v, got %v", expectedLeaves, leaves)
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
			First:     &DrawerNode{Type: "leaf", TerminalID: "term_d1"},
			Second:    &DrawerNode{Type: "leaf", TerminalID: "term_d2"},
		},
		Second: &DrawerNode{Type: "leaf", TerminalID: "term_d3"},
	}

	// Suppose term_d1 was closed (exited), leaving term_d2 and term_d3 alive.
	alive := map[string]PaneInfo{
		"term_nvim": {PaneID: "w4:p1", TerminalID: "term_nvim"},
		"term_d2":   {PaneID: "w5:p2", TerminalID: "term_d2"},
		"term_d3":   {PaneID: "w5:p3", TerminalID: "term_d3"},
	}

	pruned := pruneDrawerTree(tree, alive, "term_nvim")
	if pruned == nil || pruned.Type != "split" || pruned.Direction != "right" || pruned.Ratio != 0.4 {
		t.Fatalf("expected top-level right split to survive, got %+v", pruned)
	}
	if pruned.First == nil || pruned.First.Type != "leaf" || pruned.First.TerminalID != "term_d2" {
		t.Fatalf("expected first child to collapse to term_d2 leaf, got %+v", pruned.First)
	}
	if pruned.Second == nil || pruned.Second.Type != "leaf" || pruned.Second.TerminalID != "term_d3" {
		t.Fatalf("expected second child to be term_d3 leaf, got %+v", pruned.Second)
	}
}

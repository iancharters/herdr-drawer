package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveProfileDefaults(t *testing.T) {
	cfg := &ProfilesFile{}
	p := resolveProfile("default", cfg)
	if p.Placement != "down" || p.Scope != "anchor" || p.SizeRatio != 0.30 || !p.PersistOnHide || p.ShowCommand || p.AnchorPolicy != "generic_non_plugin" {
		t.Fatalf("unexpected defaults: %+v", p)
	}
	if len(p.AnchorApp) != 0 {
		t.Fatalf("expected empty default AnchorApp for generic_non_plugin, got %v", p.AnchorApp)
	}

	pNil := resolveProfile("missing", nil)
	if pNil.Name != "missing" || pNil.Placement != "down" || pNil.Scope != "anchor" {
		t.Fatalf("unexpected nil config resolution: %+v", pNil)
	}
}

func TestLoadProfilesFileAndResolve(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HERDR_DRAWER_CONFIG_DIR", tmp)

	// Missing file should return empty ProfilesFile without error.
	emptyCfg, err := loadProfilesFile()
	if err != nil {
		t.Fatalf("loadProfilesFile missing: %v", err)
	}
	if got := profileNameFromFlagOrDefault("", emptyCfg); got != "default" {
		t.Fatalf("expected fallback default profile name, got %q", got)
	}

	content := `
default_profile = "agentRight"

[profiles.agentRight]
placement = "right"
scope = "tab"
size_ratio = 0.35
persist_on_hide = false
command = "cursor agent --cwd {{cwd}} --alt ${cwd}"
show_command_text = true
anchor_policy = "app_in_tab_or_tab_scope"
anchor_app_match = ["nvim", "vim"]

[profiles.topDrawer]
placement = "UP"
scope = "ANCHOR"
size_ratio = 0.95
anchor_policy = "app_in_tab_or_tab_scope"

[profiles.leftDrawer]
placement = "left"
scope = "invalid_scope"
size_ratio = 0.05
anchor_policy = "invalid_policy"
anchor_app_match = ["  HX ", "", "Helix"]
`
	if err := os.WriteFile(filepath.Join(tmp, "profiles.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write profiles: %v", err)
	}
	cfg, err := loadProfilesFile()
	if err != nil {
		t.Fatalf("load profiles: %v", err)
	}
	if got := profileNameFromFlagOrDefault("", cfg); got != "agentRight" {
		t.Fatalf("default profile mismatch: %q", got)
	}
	if got := profileNameFromFlagOrDefault("  customFlag  ", cfg); got != "customFlag" {
		t.Fatalf("explicit flag profile mismatch: %q", got)
	}

	p := resolveProfile("agentRight", cfg)
	if p.Placement != "right" || p.Scope != "tab" || p.SizeRatio != 0.35 || p.PersistOnHide || !p.ShowCommand || p.AnchorPolicy != "app_in_tab_or_tab_scope" {
		t.Fatalf("unexpected profile: %+v", p)
	}
	if !reflect.DeepEqual(p.AnchorApp, []string{"nvim", "vim"}) {
		t.Fatalf("unexpected anchor apps: %+v", p.AnchorApp)
	}
	if got := renderProfileCommand(p.Command, "/tmp/proj"); got != "cursor agent --cwd /tmp/proj --alt /tmp/proj" {
		t.Fatalf("rendered command mismatch: %q", got)
	}
	if got := renderProfileCommand("", "/tmp/proj"); got != "" {
		t.Fatalf("expected empty command to render empty, got %q", got)
	}

	top := resolveProfile("topDrawer", cfg)
	if top.Placement != "up" || top.Scope != "anchor" || top.SizeRatio != 0.30 {
		t.Fatalf("expected out-of-range size_ratio to clamp to 0.30, got %+v", top)
	}
	if !reflect.DeepEqual(top.AnchorApp, []string{"nvim", "vim"}) {
		t.Fatalf("expected default anchor_app_match [nvim vim] when omitted, got %v", top.AnchorApp)
	}

	left := resolveProfile("leftDrawer", cfg)
	if left.Placement != "left" || left.Scope != "anchor" || left.SizeRatio != 0.30 || left.AnchorPolicy != "generic_non_plugin" {
		t.Fatalf("unexpected leftDrawer fallback fields: %+v", left)
	}
	if !reflect.DeepEqual(left.AnchorApp, []string{"hx", "helix"}) {
		t.Fatalf("unexpected normalized anchor_app_match: %v", left.AnchorApp)
	}

	// Invalid TOML should return an error.
	if err := os.WriteFile(filepath.Join(tmp, "profiles.toml"), []byte("invalid = [toml"), 0o644); err != nil {
		t.Fatalf("write invalid profiles: %v", err)
	}
	if _, err := loadProfilesFile(); err == nil {
		t.Fatalf("expected error loading invalid TOML")
	}
}

func TestPluginConfigDirResolution(t *testing.T) {
	t.Setenv("HERDR_DRAWER_CONFIG_DIR", "/custom/drawer/cfg")
	if got := pluginConfigDir(); got != "/custom/drawer/cfg" {
		t.Fatalf("expected HERDR_DRAWER_CONFIG_DIR, got %q", got)
	}

	t.Setenv("HERDR_DRAWER_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/custom/xdg")
	expected := filepath.Join("/custom/xdg", "herdr", "plugins", "config", pluginID)
	if got := pluginConfigDir(); got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

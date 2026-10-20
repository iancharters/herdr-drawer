package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveProfileDefaults(t *testing.T) {
	cfg := &ProfilesFile{}
	p := resolveProfile("default", cfg)
	if p.Placement != "down" || p.SizeRatio != 0.30 || !p.PersistOnHide {
		t.Fatalf("unexpected defaults: %+v", p)
	}
}

func TestLoadProfilesFileAndResolve(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HERDR_DRAWER_CONFIG_DIR", tmp)
	content := `
default_profile = "agentRight"

[profiles.agentRight]
placement = "right"
size_ratio = 0.35
persist_on_hide = false
command = "cursor agent --cwd {{cwd}}"
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
	p := resolveProfile("agentRight", cfg)
	if p.Placement != "right" || p.SizeRatio != 0.35 || p.PersistOnHide {
		t.Fatalf("unexpected profile: %+v", p)
	}
	if got := renderProfileCommand(p.Command, "/tmp/proj"); got != "cursor agent --cwd /tmp/proj" {
		t.Fatalf("rendered command mismatch: %q", got)
	}
}

package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const defaultProfileName = "default"

type ProfilesFile struct {
	DefaultProfile string                   `toml:"default_profile"`
	Profiles       map[string]ProfileConfig `toml:"profiles"`
}

type ProfileConfig struct {
	Placement     string  `toml:"placement"`
	SizeRatio     float64 `toml:"size_ratio"`
	PersistOnHide *bool   `toml:"persist_on_hide"`
	Command       string  `toml:"command"`
}

type EffectiveProfile struct {
	Name          string
	Placement     string
	SizeRatio     float64
	PersistOnHide bool
	Command       string
}

func defaultPersist() bool {
	return true
}

func pluginConfigDir() string {
	if p := strings.TrimSpace(os.Getenv("HERDR_DRAWER_CONFIG_DIR")); p != "" {
		return p
	}
	configDir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return filepath.Join(".config", "herdr", "plugins", "config", pluginID)
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "herdr", "plugins", "config", pluginID)
}

func loadProfilesFile() (*ProfilesFile, error) {
	path := filepath.Join(pluginConfigDir(), "profiles.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ProfilesFile{}, nil
		}
		return nil, err
	}
	cfg := &ProfilesFile{}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func profileNameFromFlagOrDefault(input string, cfg *ProfilesFile) string {
	if s := strings.TrimSpace(input); s != "" {
		return s
	}
	if cfg != nil {
		if s := strings.TrimSpace(cfg.DefaultProfile); s != "" {
			return s
		}
	}
	return defaultProfileName
}

func resolveProfile(profileName string, cfg *ProfilesFile) EffectiveProfile {
	out := EffectiveProfile{
		Name:          profileName,
		Placement:     "down",
		SizeRatio:     0.30,
		PersistOnHide: true,
	}
	if cfg == nil || cfg.Profiles == nil {
		return out
	}
	p, ok := cfg.Profiles[profileName]
	if !ok {
		return out
	}
	if dir := strings.ToLower(strings.TrimSpace(p.Placement)); dir == "up" || dir == "down" || dir == "left" || dir == "right" {
		out.Placement = dir
	}
	if p.SizeRatio >= 0.1 && p.SizeRatio <= 0.9 {
		out.SizeRatio = p.SizeRatio
	}
	if p.PersistOnHide != nil {
		out.PersistOnHide = *p.PersistOnHide
	}
	out.Command = strings.TrimSpace(p.Command)
	return out
}

func renderProfileCommand(cmd, cwd string) string {
	if cmd == "" {
		return ""
	}
	out := strings.ReplaceAll(cmd, "{{cwd}}", cwd)
	out = strings.ReplaceAll(out, "${cwd}", cwd)
	return out
}

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultValid(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}
	if cfg.Policy.AuthMode != 0 || cfg.Policy.TimeoutAction != "disconnect" {
		t.Fatalf("unexpected policy defaults: %+v", cfg.Policy)
	}
	if cfg.Shell.BotPrefix != "BOT-" {
		t.Fatalf("unexpected bot prefix: %q", cfg.Shell.BotPrefix)
	}
}

func TestAuthModeNormalization(t *testing.T) {
	cfg := Default()
	cfg.Policy.AuthMode = 9
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.AuthMode != 0 {
		t.Fatalf("invalid auth_mode should fall back to 0, got %d", cfg.Policy.AuthMode)
	}
}

func TestNativeRequiresToken(t *testing.T) {
	cfg := Default()
	cfg.Native.Enabled = true
	cfg.Native.AuthToken = ""
	if err := cfg.normalize(); err == nil {
		t.Fatal("expected error when native enabled without token")
	}
	cfg.Native.AuthToken = "secret"
	if err := cfg.normalize(); err != nil {
		t.Fatalf("unexpected error with token: %v", err)
	}
}

// A wildcard host conflicts with any address on the same port.
func TestPortConflictWildcard(t *testing.T) {
	cfg := Default()
	cfg.Native.Enabled = true
	cfg.Native.AuthToken = "x"
	cfg.Shell.Enabled = true
	cfg.Native.ListenPort = 8881
	cfg.Shell.ListenPort = 8881
	cfg.Native.ListenHost = "0.0.0.0"
	cfg.Shell.ListenHost = "127.0.0.1"
	if err := cfg.normalize(); err == nil {
		t.Fatal("expected wildcard/loopback port conflict")
	}
}

// An existing but empty config file falls back to defaults instead of erroring.
func TestEmptyConfigFileUsesDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(p, []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("empty config should load defaults: %v", err)
	}
	if cfg.Policy.RoundLimit == 0 || cfg.Web.ListenPort == 0 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

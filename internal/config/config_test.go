package config

import "testing"

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

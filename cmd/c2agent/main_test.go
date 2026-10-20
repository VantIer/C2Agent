package main

import (
	"bytes"
	"log"
	"net"
	"path/filepath"
	"testing"

	"c2agent/internal/config"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestResolveMode(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = "web"
	if got := resolveMode("", cfg); got != "web" {
		t.Fatalf("config mode = %q", got)
	}
	if got := resolveMode("cli", cfg); got != "cli" {
		t.Fatalf("flag mode = %q", got)
	}
}

func TestStartFileServerDisabled(t *testing.T) {
	cfg := config.Default()
	var logs bytes.Buffer
	if srv := startFileServer(cfg, log.New(&logs, "", 0)); srv != nil {
		srv.Close()
		t.Fatal("disabled file server should not start")
	}
}

func TestStartFileServerInvalidPath(t *testing.T) {
	cfg := config.Default()
	cfg.FileServer.Enabled = true
	cfg.FileServer.ListenHost = "127.0.0.1"
	cfg.FileServer.ListenPort = freePort(t)
	cfg.FileServer.Path = filepath.Join(t.TempDir(), "missing")

	var logs bytes.Buffer
	if srv := startFileServer(cfg, log.New(&logs, "", 0)); srv != nil {
		srv.Close()
		t.Fatal("invalid path should disable the file server")
	}
	if logs.Len() == 0 {
		t.Fatal("expected a disabled log message")
	}
}

func TestStartFileServerSuccess(t *testing.T) {
	cfg := config.Default()
	cfg.FileServer.Enabled = true
	cfg.FileServer.ListenHost = "127.0.0.1"
	cfg.FileServer.ListenPort = freePort(t)
	cfg.FileServer.Path = t.TempDir()

	var logs bytes.Buffer
	srv := startFileServer(cfg, log.New(&logs, "", 0))
	if srv == nil {
		t.Fatalf("file server did not start; logs: %s", logs.String())
	}
	defer srv.Close()
	if srv.Addr() == nil {
		t.Fatal("file server has no address")
	}
}

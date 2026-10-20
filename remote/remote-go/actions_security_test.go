package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathIsInside(t *testing.T) {
	root := t.TempDir()
	if !pathIsInside(root, filepath.Join(root, "sub")) {
		t.Fatal("child should be inside parent")
	}
	if pathIsInside(filepath.Join(root, "a"), filepath.Join(root, "ab")) {
		t.Fatal("sibling prefix must not count as inside")
	}
}

func TestReadFileLineRangeCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.txt")
	var b strings.Builder
	for i := 1; i <= 600; i++ {
		b.WriteString("line\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out := readFile(path, "1", "999")
	if got := strings.Count(out, "\n"); got != 512 {
		t.Fatalf("got %d lines, want 512", got)
	}
}

func TestCapOutput(t *testing.T) {
	out := capOutput(strings.Repeat("a", maxOutputBytes+10), false)
	if len(out) > maxOutputBytes {
		t.Fatalf("output is %d bytes, want <= %d", len(out), maxOutputBytes)
	}
	if !strings.HasSuffix(out, "[... output truncated at 8 MiB]") {
		t.Fatal("missing truncation marker")
	}
}

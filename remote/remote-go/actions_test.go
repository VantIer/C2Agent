package main

import "testing"

func TestApplyStringEditUniqueReplace(t *testing.T) {
	out, errMsg := applyStringEdit("a\nb\nc\n", "b\n", "x\ny\n")
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if out != "a\nx\ny\nc\n" {
		t.Fatalf("got %q, want %q", out, "a\nx\ny\nc\n")
	}
}

func TestApplyStringEditDelete(t *testing.T) {
	out, errMsg := applyStringEdit("keep\ndrop\nkeep2\n", "drop\n", "")
	if errMsg != "" || out != "keep\nkeep2\n" {
		t.Fatalf("delete: out=%q err=%q", out, errMsg)
	}
}

func TestApplyStringEditNotFound(t *testing.T) {
	if _, errMsg := applyStringEdit("a\nb\n", "zzz", "y"); errMsg == "" {
		t.Fatal("expected an error for a missing old_text")
	}
}

func TestApplyStringEditAmbiguous(t *testing.T) {
	if _, errMsg := applyStringEdit("x\nx\n", "x\n", "y\n"); errMsg == "" {
		t.Fatal("expected an error for a non-unique old_text")
	}
}

func TestApplyStringEditOverlappingIsAmbiguous(t *testing.T) {
	if _, errMsg := applyStringEdit("aaa", "aa", "b"); errMsg == "" {
		t.Fatal("expected an error for an overlapping non-unique old_text")
	}
}

func TestApplyStringEditEmptyOldText(t *testing.T) {
	if _, errMsg := applyStringEdit("a\n", "", "y"); errMsg == "" {
		t.Fatal("expected an error for an empty old_text")
	}
}

func TestApplyStringEditCRLFTolerant(t *testing.T) {
	out, errMsg := applyStringEdit("a\r\nb\r\nc\r\n", "b\n", "x\n")
	if errMsg != "" || out != "a\r\nx\r\nc\r\n" {
		t.Fatalf("crlf: out=%q err=%q", out, errMsg)
	}
}

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func copyPath(src, dest string) string {
	info, err := os.Stat(src)
	if err != nil {
		return "Source not found: " + src
	}
	if info.IsDir() {
		if pathIsInside(src, dest) {
			return "Error: destination is inside source: " + dest
		}
		if err := copyTree(src, dest); err != nil {
			return "Error copying: " + err.Error()
		}
	} else {
		if dir := filepath.Dir(dest); dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0o755)
		}
		if err := copyFile(src, dest); err != nil {
			return "Error copying: " + err.Error()
		}
	}
	return fmt.Sprintf("Successfully copied: %s -> %s", src, dest)
}

// pathIsInside reports whether child is parent itself or a descendant of it.
// The check is lexical (the destination may not exist yet).
func pathIsInside(parent, child string) bool {
	p, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	c, err := filepath.Abs(child)
	if err != nil {
		return false
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func movePath(src, dest string) string {
	info, err := os.Stat(src)
	if err != nil {
		return "Source not found: " + src
	}
	if info.IsDir() && pathIsInside(src, dest) {
		return "Error: destination is inside source: " + dest
	}
	if dir := filepath.Dir(dest); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.Rename(src, dest); err == nil {
		return fmt.Sprintf("Successfully moved: %s -> %s", src, dest)
	}
	// Cross-device fallback: copy then remove.
	info, _ = os.Stat(src)
	if info != nil && info.IsDir() {
		if err := copyTree(src, dest); err != nil {
			return "Error moving: " + err.Error()
		}
		if err := os.RemoveAll(src); err != nil {
			return "Error moving: " + err.Error()
		}
	} else {
		if err := copyFile(src, dest); err != nil {
			return "Error moving: " + err.Error()
		}
		if err := os.Remove(src); err != nil {
			return "Error moving: " + err.Error()
		}
	}
	return fmt.Sprintf("Successfully moved: %s -> %s", src, dest)
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyTree(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dest)
	}
	if err := os.MkdirAll(dest, info.Mode()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dest, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

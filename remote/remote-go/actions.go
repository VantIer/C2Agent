package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func arg(params []string, i int) string {
	if i < len(params) {
		return params[i]
	}
	return ""
}

func runAction(cmd uint8, params []string, cmdTimeout time.Duration) string {
	switch cmd {
	case cmdGetCwd:
		return getCwd()
	case cmdListDir:
		return listDir(arg(params, 0))
	case cmdMakeDir:
		return makeDir(arg(params, 0))
	case cmdCreateFile:
		return createFile(arg(params, 0))
	case cmdDeleteDir:
		return deletePath(arg(params, 0), true)
	case cmdDeleteFile:
		return deletePath(arg(params, 0), false)
	case cmdRenameDir, cmdRenameFile:
		return renamePath(arg(params, 0), arg(params, 1))
	case cmdReadFile:
		return readFile(arg(params, 0), arg(params, 1), arg(params, 2))
	case cmdWriteFile:
		return writeFile(arg(params, 0), arg(params, 1))
	case cmdEditFile:
		return editFile(arg(params, 0), arg(params, 1), arg(params, 2))
	case cmdCopy:
		return copyPath(arg(params, 0), arg(params, 1))
	case cmdMove:
		return movePath(arg(params, 0), arg(params, 1))
	case cmdExecCmd:
		return runCmd(arg(params, 0), cmdTimeout)
	}
	return fmt.Sprintf("Error: Unknown cmd: %#x", cmd)
}

func getCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "Error getting cwd: " + err.Error()
	}
	if abs, err := filepath.Abs(wd); err == nil {
		return abs
	}
	return wd
}

func listDir(path string) string {
	if path == "" {
		path = "."
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "Path does not exist: " + path
		}
		return "Error listing directory: " + err.Error()
	}
	if !info.IsDir() {
		return path + " is a file"
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "Error listing directory: " + err.Error()
	}
	var b strings.Builder
	for _, e := range entries {
		typ := "FILE"
		size := int64(0)
		if e.IsDir() {
			typ = "DIR"
		} else if fi, err := e.Info(); err == nil {
			size = fi.Size()
		}
		fmt.Fprintf(&b, "%-6s %12d %s\n", typ, size, e.Name())
	}
	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		return "Empty directory"
	}
	return out
}

func makeDir(path string) string {
	if _, err := os.Stat(path); err == nil {
		return "Directory already exists: " + path
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "Error creating directory: " + err.Error()
	}
	return "Successfully created directory: " + path
}

func createFile(path string) string {
	if _, err := os.Stat(path); err == nil {
		return "File already exists: " + path
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "Error creating file: " + err.Error()
	}
	_ = f.Close()
	return "Successfully created file: " + path
}

func deletePath(path string, wantDir bool) string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "Path does not exist: " + path
		}
		return "Error deleting: " + err.Error()
	}
	if info.IsDir() != wantDir {
		kind := "file"
		if wantDir {
			kind = "directory"
		}
		return "Error: not a " + kind + ": " + path
	}
	if err := os.RemoveAll(path); err != nil {
		return "Error deleting: " + err.Error()
	}
	return "Successfully deleted: " + path
}

func renamePath(path, newName string) string {
	if newName == "" || strings.ContainsAny(newName, `/\`) {
		return "Error: new_name must be a bare name without path separators"
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "Path does not exist: " + path
		}
		return "Error renaming: " + err.Error()
	}
	newPath := filepath.Join(filepath.Dir(path), newName)
	if _, err := os.Stat(newPath); err == nil {
		return "Target name already exists: " + newName
	}
	if err := os.Rename(path, newPath); err != nil {
		return "Error renaming: " + err.Error()
	}
	return fmt.Sprintf("Successfully renamed: %s -> %s", path, newName)
}

//go:build windows

package main

// decodeConsole converts bytes emitted by a Windows child process on a pipe
// into UTF-8. Console programs write bytes in the active console/OEM code
// page (e.g. CP936 on Simplified Chinese Windows), not UTF-8, so a raw
// passthrough would be replaced by U+FFFD when the control end JSON-encodes
// the result.
//
// Implemented via kernel32 through syscall to keep the agent free of
// third-party dependencies.

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

// shellCmd runs command through cmd.exe. The command line is supplied verbatim
// via SysProcAttr.CmdLine so Go's CRT-style argument escaping does not mangle
// embedded quotes: exec.Command("cmd", "/C", command) would rewrite
// `"C:\Program Files\x.exe"` as `\"C:\Program Files\x.exe\"`, which cmd.exe
// then rejects ("is not recognized as an internal or external command").
func shellCmd(ctx context.Context, command string) *exec.Cmd {
	c := exec.CommandContext(ctx, "cmd.exe")
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd.exe /c " + command}
	return c
}

// killTree terminates the command's whole process tree. CommandContext alone
// only kills cmd.exe, leaving its children running and their inherited stdout
// pipe open (which would make CombinedOutput block).
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
	return cmd.Process.Kill()
}

const cpUTF8 = 65001

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleOutputCP  = kernel32.NewProc("GetConsoleOutputCP")
	procGetOEMCP            = kernel32.NewProc("GetOEMCP")
	procMultiByteToWideChar = kernel32.NewProc("MultiByteToWideChar")
	procWideCharToMultiByte = kernel32.NewProc("WideCharToMultiByte")
)

// consoleCodePage returns the code page used for redirected console output,
// falling back to the OEM code page when the process has no console.
func consoleCodePage() uint32 {
	if cp, _, _ := procGetConsoleOutputCP.Call(); cp != 0 {
		return uint32(cp)
	}
	if cp, _, _ := procGetOEMCP.Call(); cp != 0 {
		return uint32(cp)
	}
	return 0
}

func decodeConsole(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	cp := consoleCodePage()
	if cp == 0 || cp == cpUTF8 {
		return b
	}

	// code page bytes -> UTF-16
	n, _, _ := procMultiByteToWideChar.Call(
		uintptr(cp), 0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		0, 0,
	)
	if n == 0 {
		return b
	}
	wide := make([]uint16, int(n))
	procMultiByteToWideChar.Call(
		uintptr(cp), 0,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(unsafe.Pointer(&wide[0])), uintptr(n),
	)

	// UTF-16 -> UTF-8
	m, _, _ := procWideCharToMultiByte.Call(
		uintptr(cpUTF8), 0,
		uintptr(unsafe.Pointer(&wide[0])), uintptr(n),
		0, 0, 0, 0,
	)
	if m == 0 {
		return b
	}
	out := make([]byte, int(m))
	procWideCharToMultiByte.Call(
		uintptr(cpUTF8), 0,
		uintptr(unsafe.Pointer(&wide[0])), uintptr(n),
		uintptr(unsafe.Pointer(&out[0])), uintptr(m), 0, 0,
	)
	return out
}

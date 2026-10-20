package shell

import "strings"

// Execution-environment identifiers for shell bots. The environment is the
// command interpreter actually attached to the reverse shell; it decides how a
// high-level action is turned into a command. Native agents do not use it.
const (
	EnvPowerShell = "PowerShell"
	EnvCmd        = "cmd"
	EnvBash       = "bash"
	EnvSh         = "sh"
	EnvUnknown    = "Unknown"
)

// target is a shell bot's OS plus command interpreter.
type target struct {
	os  string
	env string
}

func (t target) isWindows() bool { return t.os == "Windows" }

// psHost reports whether commands run directly in a PowerShell interpreter
// (vs. being piped through `powershell -EncodedCommand` from a cmd.exe host).
func (t target) psHost() bool { return t.isWindows() && t.env == EnvPowerShell }

// posixQuote single-quotes a string for POSIX shells.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// posixPath quotes a filesystem path and protects a relative name beginning
// with '-' from being parsed as an option by commands such as head/tail/rm.
func posixPath(s string) string {
	if strings.HasPrefix(s, "-") {
		s = "./" + s
	}
	return posixQuote(s)
}

// psLiteral single-quotes a string for a PowerShell script. Windows commands
// are either executed directly by a PowerShell host or passed via
// -EncodedCommand (cmd.exe host), so the value is evaluated exactly once:
// '$', '%' and '`' are literal and need no special handling.
func psLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

const (
	posixChunk = 100000
	// Guards against emitting a single shell command that exceeds the host
	// shell's command-line / input length limits (base64 payload bytes).
	posixCmdLimit = 900000
	// winCmdLimit is cmd.exe's command-line budget (~8 KB) for the
	// "powershell -NoProfile -EncodedCommand <b64>" launcher used on cmd.exe hosts.
	winCmdLimit = 7000
	// winPSCmdLimit bounds a command executed directly by a PowerShell host: it
	// travels as one line into the reverse shell, whose read buffer is 64 KB.
	winPSCmdLimit = 32000
	// readFileByteCap bounds how many bytes a whole-file read pulls from the
	// head of a file (enough for 51200 UTF-8 characters), so an enormous file
	// cannot exhaust the controlled end's memory.
	readFileByteCap = 51200 * 4

	// Segmented transfer chunk sizes: each chunk is moved by ONE shell command,
	// so it must fit the host shell's command-line limit once base64-encoded.
	// The control end drives the offset loop; the shell bot only reads/appends.
	posixTransferChunk = 32768
	// cmd host: the base64 chunk must survive -EncodedCommand inflation (~8/3x).
	winCmdTransferChunk = 1500
	// PowerShell host: the chunk travels directly (no base64 command inflation).
	winPSTransferChunk = 8192
	// Read chunks are not embedded in the command (only the response carries
	// data), so downloads can use a large chunk regardless of the host shell.
	winReadChunk = 32768

	// Fixed sentinels framing Windows replies whose payload the control end
	// parses strictly (whole-file content, chunk content, file size). The read
	// payloads are base64 (alphabet A-Za-z0-9+/=) and the size payload is
	// decimal, so a sentinel containing '_' can never occur inside the data: the
	// frame cannot collide with the payload it wraps. This lets the parser
	// ignore any surrounding stream noise (e.g. a PowerShell CLIXML progress
	// block) that a merged stderr would otherwise inject into the reply.
	winReadBegin    = "__C2READ_B__"
	winReadEnd      = "__C2READ_E__"
	winReadMissing  = "__C2READ_M__"
	winSizeBegin    = "__C2SIZE_B__"
	winSizeEnd      = "__C2SIZE_E__"
	winSizeMissing  = "__C2SIZE_M__"
	winChunkBegin   = "__C2CHUNK_B__"
	winChunkEnd     = "__C2CHUNK_E__"
	winChunkMissing = "__C2CHUNK_M__"
	// Line-range read framing. RANGE means the requested start line is past
	// EOF; TOOBIG means the selected bytes exceed the read cap.
	winRangeMissing = "__C2RANGE_M__"
	winRangeTooBig  = "__C2RANGE_T__"
)

// transferChunkSize is the plain-byte chunk size used for segmented upload
// (the base64 payload is embedded in one command, so it is shell-limited).
func (t target) transferChunkSize() int {
	switch {
	case t.psHost():
		return winPSTransferChunk
	case t.isWindows():
		return winCmdTransferChunk
	default:
		return posixTransferChunk
	}
}

// readChunkSize is the plain-byte chunk size used for segmented download (the
// payload is only in the reply, so it is not bounded by the command line).
func (t target) readChunkSize() int {
	if t.isWindows() {
		return winReadChunk
	}
	return posixTransferChunk
}

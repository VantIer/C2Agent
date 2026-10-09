package shell

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runOnHost executes a generated Windows shell command the way the reverse
// shell would: a PowerShell host evaluates it with `iex`; a cmd.exe host gets
// it via `cmd /c`.
func runOnHost(t *testing.T, tgt target, cmd string) string {
	t.Helper()
	var c *exec.Cmd
	if tgt.env == EnvPowerShell {
		c = exec.Command("powershell", "-NoProfile", "-Command", cmd)
	} else {
		c = exec.Command("cmd", "/c", cmd)
	}
	// Only stdout reaches the reverse-shell socket; PowerShell may emit a
	// progress CLIXML on stderr, which the protocol never sees.
	out, err := c.Output()
	if err != nil {
		t.Fatalf("host %s failed: %v\ncmd: %s\nout: %s", tgt.env, err, cmd, out)
	}
	return strings.TrimSpace(string(out))
}

// TestGeneratedWindowsCommandsExecute runs the real command builders through
// real PowerShell and cmd.exe hosts to prove both environment paths work and
// that paths containing '$' survive. Skipped off Windows.
func TestGeneratedWindowsCommandsExecute(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires a Windows host with powershell.exe and cmd.exe")
	}
	if testing.Short() {
		t.Skip("skipping subprocess-heavy test in -short mode")
	}
	dir := t.TempDir()
	// A '$' in the name proves the single-quoting + -EncodedCommand path is safe.
	path := filepath.Join(dir, "$c2_test.txt")
	want := []byte("line1\nline2\nline3\n") // LF-only, must round-trip unchanged
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tgt := range []target{
		{os: "Windows", env: EnvPowerShell},
		{os: "Windows", env: EnvCmd},
	} {
		t.Run(tgt.env, func(t *testing.T) {
			// Whole-file read (base64) then decode.
			readCmd, err := readFileCmd(tgt, path, "", "")
			if err != nil {
				t.Fatal(err)
			}
			gotB64 := runOnHost(t, tgt, readCmd)
			got, derr := base64.StdEncoding.DecodeString(gotB64)
			if derr != nil {
				t.Fatalf("read output is not base64 (%q): %v", gotB64, derr)
			}
			if string(got) != string(want) {
				t.Fatalf("read mismatch: got %q want %q", got, want)
			}

			// file size.
			sizeCmd, err := fileSizeCmd(tgt, path)
			if err != nil {
				t.Fatal(err)
			}
			if s := runOnHost(t, tgt, sizeCmd); s != "18" {
				t.Fatalf("fileSize = %q, want 18", s)
			}

			// write_file round trip (content with spaces/newlines).
			outPath := filepath.Join(dir, "out_"+tgt.env+".txt")
			content := "hello\nworld $VAR\n"
			wb, err := writeBase64Cmd(tgt, outPath, base64.StdEncoding.EncodeToString([]byte(content)))
			if err != nil {
				t.Fatal(err)
			}
			runOnHost(t, tgt, wb)
			rewritten, rerr := os.ReadFile(outPath)
			if rerr != nil {
				t.Fatalf("out file not written: %v", rerr)
			}
			if string(rewritten) != content {
				t.Fatalf("write mismatch: got %q want %q", rewritten, content)
			}

			// list_dir must render the Windows table formatListing understands.
			lsCmd, err := listDirCmd(tgt, dir)
			if err != nil {
				t.Fatal(err)
			}
			listing := formatListing(runOnHost(t, tgt, lsCmd))
			if !strings.Contains(listing, "FILE") || !strings.Contains(listing, "c2_test.txt") {
				t.Fatalf("listing not normalized: %q", listing)
			}
			// A missing path must produce a locale-independent "Error:".
			missCmd, err := listDirCmd(tgt, filepath.Join(dir, "nope_"+tgt.env))
			if err != nil {
				t.Fatal(err)
			}
			if out := strings.TrimSpace(runOnHost(t, tgt, missCmd)); !strings.HasPrefix(out, "Error") {
				t.Fatalf("list_dir on a missing path = %q, want an Error line", out)
			}
			// An empty directory must report "Empty directory".
			emptyDir := filepath.Join(dir, "emptydir_"+tgt.env)
			if err := os.Mkdir(emptyDir, 0o755); err != nil {
				t.Fatal(err)
			}
			emptyCmd, err := listDirCmd(tgt, emptyDir)
			if err != nil {
				t.Fatal(err)
			}
			if out := strings.TrimSpace(runOnHost(t, tgt, emptyCmd)); out != "Empty directory" {
				t.Fatalf("empty dir list = %q, want %q", out, "Empty directory")
			}
			// fileSize on a missing path must error rather than report 0.
			sizeMiss, err := fileSizeCmd(tgt, filepath.Join(dir, "absent_"+tgt.env))
			if err != nil {
				t.Fatal(err)
			}
			if out := strings.TrimSpace(runOnHost(t, tgt, sizeMiss)); !strings.HasPrefix(out, "Error") {
				t.Fatalf("fileSize on a missing path = %q, want an Error", out)
			}

			// rename over an existing destination must overwrite it (upload and
			// edit_file move a temp file onto an already-existing target).
			src := filepath.Join(dir, "src_"+tgt.env)
			dst := filepath.Join(dir, "dst_"+tgt.env)
			if err := os.WriteFile(src, []byte("SRC"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte("OLD"), 0o644); err != nil {
				t.Fatal(err)
			}
			rn, err := renameCmd(tgt, src, filepath.Base(dst))
			if err != nil {
				t.Fatal(err)
			}
			runOnHost(t, tgt, rn)
			if got, _ := os.ReadFile(dst); string(got) != "SRC" {
				t.Fatalf("rename did not overwrite destination: %q", got)
			}

			// copy into a not-yet-existing directory must create the parent.
			cpSrc := filepath.Join(dir, "cp_src.txt")
			if err := os.WriteFile(cpSrc, []byte("COPIED"), 0o644); err != nil {
				t.Fatal(err)
			}
			cpDst := filepath.Join(dir, "newsub", "cp_dst.txt")
			cp, err := copyCmd(tgt, cpSrc, cpDst)
			if err != nil {
				t.Fatal(err)
			}
			runOnHost(t, tgt, cp)
			if got, rerr := os.ReadFile(cpDst); rerr != nil || string(got) != "COPIED" {
				t.Fatalf("copy did not create parent/target: %v %q", rerr, got)
			}

			// create_file must NOT truncate an existing file, and must create
			// parent directories for a new one.
			keep := filepath.Join(dir, "keep_"+tgt.env+".txt")
			if err := os.WriteFile(keep, []byte("KEEP"), 0o644); err != nil {
				t.Fatal(err)
			}
			cf, err := createFileCmd(tgt, keep)
			if err != nil {
				t.Fatal(err)
			}
			runOnHost(t, tgt, cf)
			if got, _ := os.ReadFile(keep); string(got) != "KEEP" {
				t.Fatalf("create_file truncated existing file: %q", got)
			}
			fresh := filepath.Join(dir, "sub2", "fresh.txt")
			cf2, err := createFileCmd(tgt, fresh)
			if err != nil {
				t.Fatal(err)
			}
			runOnHost(t, tgt, cf2)
			if info, serr := os.Stat(fresh); serr != nil || info.Size() != 0 {
				t.Fatalf("create_file did not create empty file: %v", serr)
			}

			// delete_file must refuse a directory; delete_dir must refuse a file.
			ddir := filepath.Join(dir, "del_dir_"+tgt.env)
			if err := os.Mkdir(ddir, 0o755); err != nil {
				t.Fatal(err)
			}
			delFile := filepath.Join(dir, "del_file_"+tgt.env)
			if err := os.WriteFile(delFile, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			dfCmd, err := deleteCmd(tgt, ddir, false) // delete_file on a directory
			if err != nil {
				t.Fatal(err)
			}
			if out := runOnHost(t, tgt, dfCmd); !strings.Contains(out, "Error") {
				t.Fatalf("delete_file on a directory should report an error, got %q", out)
			}
			if _, serr := os.Stat(ddir); serr != nil {
				t.Fatal("delete_file removed a directory it should have refused")
			}
			ddCmd, err := deleteCmd(tgt, delFile, true) // delete_dir on a file
			if err != nil {
				t.Fatal(err)
			}
			if out := runOnHost(t, tgt, ddCmd); !strings.Contains(out, "Error") {
				t.Fatalf("delete_dir on a file should report an error, got %q", out)
			}
			if _, serr := os.Stat(delFile); serr != nil {
				t.Fatal("delete_dir removed a file it should have refused")
			}
			okCmd, err := deleteCmd(tgt, delFile, false)
			if err != nil {
				t.Fatal(err)
			}
			runOnHost(t, tgt, okCmd)
			if _, serr := os.Stat(delFile); !os.IsNotExist(serr) {
				t.Fatal("delete_file did not remove a regular file")
			}

			// A failing operation must be caught and returned as text, not tear
			// down the shell host (a propagated terminating error would).
			badPath := filepath.Join(cpSrc, "child", "x.txt") // cpSrc is a file
			badCmd, err := writeBase64Cmd(tgt, badPath, base64.StdEncoding.EncodeToString([]byte("z")))
			if err != nil {
				t.Fatal(err)
			}
			if out := runOnHost(t, tgt, badCmd); strings.TrimSpace(out) == "" {
				t.Fatal("failing write produced no error output (terminating error escaped?)")
			}

			// edit_file read half: the sentinel-wrapped read must round-trip
			// through extractWrappedEdit on a real host.
			begin, end, missing := "__B__", "__E__", "__M__"
			wrapped, err := readFileWrappedCmd(tgt, path, begin, end, missing, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			content, perr := extractWrappedEdit(runOnHost(t, tgt, wrapped), begin, end, missing)
			if perr != "" || content != string(want) {
				t.Fatalf("wrapped read round-trip failed: err=%q content=%q", perr, content)
			}

			// Missing file must yield the missing sentinel, not an error/teardown.
			absent := filepath.Join(dir, "does_not_exist_"+tgt.env)
			wrappedMiss, err := readFileWrappedCmd(tgt, absent, begin, end, missing, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if _, perr := extractWrappedEdit(runOnHost(t, tgt, wrappedMiss), begin, end, missing); perr == "" {
				t.Fatal("missing file did not produce a read error")
			}
		})
	}
}

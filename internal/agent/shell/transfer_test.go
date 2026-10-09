package shell

import (
	"bufio"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"c2agent/internal/agent"
)

// memFS is a mutex-guarded in-memory filesystem used by the fake shell.
type memFS struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemFS() *memFS { return &memFS{m: map[string][]byte{}} }

func (f *memFS) size(p string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.m[p]
	return len(b), ok
}

func (f *memFS) get(p string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.m[p]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), b...), true
}

func (f *memFS) set(p string, b []byte) {
	f.mu.Lock()
	f.m[p] = append([]byte(nil), b...)
	f.mu.Unlock()
}

func (f *memFS) appendData(p string, b []byte) {
	f.mu.Lock()
	f.m[p] = append(f.m[p], b...)
	f.mu.Unlock()
}

func (f *memFS) del(p string) {
	f.mu.Lock()
	delete(f.m, p)
	f.mu.Unlock()
}

var (
	reSize     = regexp.MustCompile(`^wc -c < '([^']*)'$`)
	reTail     = regexp.MustCompile(`^tail -c \+(\d+) '([^']*)' \| head -c (\d+) \| base64$`)
	reEditPath = regexp.MustCompile(`^if \[ -f '([^']*)' \] && \[ -r`)
	reToken    = regexp.MustCompile(`__C2EDIT_[BEM]_[0-9a-f]+__`)
	reCreate   = regexp.MustCompile(`^mkdir -p "\$\(dirname '([^']*)'\)"; : > '([^']*)'$`)
	reAppend   = regexp.MustCompile(`^printf '%s' '([^']*)' \| base64 -d >> '([^']*)'$`)
	reWrite    = regexp.MustCompile(`^printf '%s' '([^']*)' \| base64 -d > '([^']*)'$`)
	reMV       = regexp.MustCompile(`^mv '([^']*)' '([^']*)'$`)
	reRM       = regexp.MustCompile(`^rm -f '([^']*)'$`)
)

// fakeShellFS emulates a POSIX reverse shell for exactly the commands the shell
// backend issues for edit_file and segmented upload/download.
func fakeShellFS(conn net.Conn, fs *memFS) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	out := func(s string) { _, _ = conn.Write([]byte(s)) }
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		// Emit an interactive prompt before each reply, like `bash -i`. It must
		// be stripped from command output but never corrupt payloads.
		out("fake$ \n")
		if strings.HasPrefix(line, "echo __C2AGENT_") {
			out(strings.TrimPrefix(line, "echo ") + "\n")
			continue
		}
		if strings.HasPrefix(line, "uname") {
			out("Linux fake\n")
			continue
		}
		if line == "hostname" {
			out("fake-host\n")
			continue
		}
		if m := reSize.FindStringSubmatch(line); m != nil {
			n, _ := fs.size(m[1])
			out(strconv.Itoa(n) + "\n")
			continue
		}
		if m := reTail.FindStringSubmatch(line); m != nil {
			off, _ := strconv.Atoi(m[1])
			length, _ := strconv.Atoi(m[3])
			data, _ := fs.get(m[2])
			start := off - 1
			if start > len(data) {
				start = len(data)
			}
			end := start + length
			if end > len(data) {
				end = len(data)
			}
			out(base64.StdEncoding.EncodeToString(data[start:end]) + "\n")
			continue
		}
		if strings.Contains(line, "__C2EDIT_B_") {
			toks := reToken.FindAllString(line, -1)
			pm := reEditPath.FindStringSubmatch(line)
			if len(toks) >= 3 && pm != nil {
				if data, ok := fs.get(pm[1]); ok {
					out(toks[0] + "\n" + base64.StdEncoding.EncodeToString(data) + "\n" + toks[1] + "\n")
				} else {
					out(toks[2] + "\n")
				}
			}
			continue
		}
		if m := reCreate.FindStringSubmatch(line); m != nil {
			fs.set(m[2], nil)
			continue
		}
		if m := reAppend.FindStringSubmatch(line); m != nil {
			if data, derr := base64.StdEncoding.DecodeString(m[1]); derr == nil {
				fs.appendData(m[2], data)
			}
			continue
		}
		if m := reWrite.FindStringSubmatch(line); m != nil {
			if data, err := base64.StdEncoding.DecodeString(m[1]); err == nil {
				fs.set(m[2], data)
			}
			continue
		}
		if m := reMV.FindStringSubmatch(line); m != nil {
			if data, ok := fs.get(m[1]); ok {
				fs.set(m[2], data)
				fs.del(m[1])
			}
			continue
		}
		if m := reRM.FindStringSubmatch(line); m != nil {
			fs.del(m[1])
			continue
		}
	}
}

func startShellBot(t *testing.T, fs *memFS) *agent.Registry {
	t.Helper()
	reg := agent.NewRegistry()
	srv := NewServer(ServerOptions{
		Registry:      reg,
		Host:          "127.0.0.1",
		Port:          0,
		BotPrefix:     "BOT-",
		CmdTimeout:    10 * time.Second,
		TimeoutAction: "disconnect",
		QueueCapacity: 16,
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	go func() {
		conn, err := net.Dial("tcp", srv.Addr().String())
		if err != nil {
			return
		}
		fakeShellFS(conn, fs)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if reg.Get("BOT-001") != nil {
			return reg
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("shell bot was not registered")
	return nil
}

func TestFakeShellCommandRegexes(t *testing.T) {
	if c, _ := truncateCreateCmd("Linux", "/tmp/x.tmp"); !reCreate.MatchString(c) {
		t.Fatalf("create regexp: %q", c)
	}
	if c, _ := appendBase64Cmd("Linux", "/tmp/x.tmp", "AAAA"); !reAppend.MatchString(c) {
		t.Fatalf("append regexp: %q", c)
	}
	if c, _ := writeBase64Cmd("Linux", "/tmp/x.tmp", "AAAA"); !reWrite.MatchString(c) {
		t.Fatalf("write regexp: %q", c)
	}
	if c, _ := renameCmd("Linux", "/tmp/x.tmp", "x"); !reMV.MatchString(c) {
		t.Fatalf("mv regexp: %q", c)
	}
	if c, _ := fileSizeCmd("Linux", "/tmp/x"); !reSize.MatchString(c) {
		t.Fatalf("size regexp: %q", c)
	}
	if c, _ := readChunkCmd("Linux", "/tmp/x", 0, 4); !reTail.MatchString(c) {
		t.Fatalf("tail regexp: %q", c)
	}
}

func TestShellEditFileSentinelEndToEnd(t *testing.T) {
	fs := newMemFS()
	fs.set("/tmp/f.txt", []byte("hello\nworld\n"))
	reg := startShellBot(t, fs)
	ag := reg.Get("BOT-001")

	job := agent.NewJob(agent.JobAction)
	job.Action = "edit_file"
	job.Params = map[string]any{"path": "/tmp/f.txt", "old_text": "world\n", "new_text": "there\n"}
	if err := ag.Enqueue(job); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	r := <-job.Result
	if r.Err != nil {
		t.Fatalf("edit error: %v", r.Err)
	}
	if !strings.HasPrefix(r.Output, "Successfully edited") {
		t.Fatalf("unexpected result: %q", r.Output)
	}
	if got, _ := fs.get("/tmp/f.txt"); string(got) != "hello\nthere\n" {
		t.Fatalf("content = %q, want %q", got, "hello\nthere\n")
	}
}

func TestShellDownloadStreaming(t *testing.T) {
	fs := newMemFS()
	big := make([]byte, posixTransferChunk*2+1234)
	for i := range big {
		big[i] = byte(i * 7)
	}
	fs.set("/tmp/big.bin", big)
	reg := startShellBot(t, fs)
	ag := reg.Get("BOT-001")

	job := agent.NewJob(agent.JobDownload)
	job.SrcPath = "/tmp/big.bin"
	job.DownloadDir = t.TempDir()
	if err := ag.Enqueue(job); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	r := <-job.Result
	if r.Err != nil {
		t.Fatalf("download error: %v", r.Err)
	}
	got, err := os.ReadFile(r.Output)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if string(got) != string(big) {
		t.Fatalf("downloaded %d bytes, want %d", len(got), len(big))
	}
}

func TestShellUploadStreaming(t *testing.T) {
	fs := newMemFS()
	reg := startShellBot(t, fs)
	ag := reg.Get("BOT-001")

	big := make([]byte, posixTransferChunk*2+777)
	for i := range big {
		big[i] = byte(i * 13)
	}
	local := filepath.Join(t.TempDir(), "local.bin")
	if err := os.WriteFile(local, big, 0o644); err != nil {
		t.Fatalf("write local: %v", err)
	}

	job := agent.NewJob(agent.JobUpload)
	job.LocalPath = local
	job.DestPath = "/tmp/up/remote.bin"
	if err := ag.Enqueue(job); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	r := <-job.Result
	if r.Err != nil {
		t.Fatalf("upload error: %v", r.Err)
	}
	got, ok := fs.get("/tmp/up/remote.bin")
	if !ok || string(got) != string(big) {
		t.Fatalf("remote content mismatch (%d bytes)", len(got))
	}
}

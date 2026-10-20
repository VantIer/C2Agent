package shell

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"c2agent/internal/agent"
	"c2agent/internal/limits"
)

const (
	// shutdownGrace bounds how long Backend.Shutdown waits for the controlled
	// end to close the socket on its own before the C2 forces it shut.
	shutdownGrace = 1 * time.Second
	// maxDrainLineBuf caps the partial-line buffer while a timed-out command is
	// being drained: its content is discarded anyway, so a runaway command
	// cannot grow memory without bound.
	maxDrainLineBuf = 32 * 1024
	writeTimeout    = 10 * time.Second
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
	bytes       int
	truncated   bool
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
	if err := b.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
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
			} else if b.pending != nil && !b.pending.draining && len(b.lineBuf) > limits.MaxOutputBytes {
				b.pending.truncated = true
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
			if p.truncated {
				out += limits.OutputTruncatedMarker
			}
			select {
			case p.ch <- shellResult{out: out}:
			default:
			}
			b.lineBuf = ""
			continue
		}
		if b.pending != nil && !b.pending.draining {
			lineLen := len(line) + 1
			if b.pending.bytes+lineLen <= limits.MaxOutputBytes {
				b.pending.lines = append(b.pending.lines, line)
				b.pending.bytes += lineLen
			} else {
				b.pending.truncated = true
			}
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
			var content, perr string
			if isWholeFileRead(params) {
				content, perr = extractWrappedEdit(out, winReadBegin, winReadEnd, winReadMissing)
			} else {
				content, perr = extractWrappedRange(out, winReadBegin, winReadEnd, winReadMissing,
					winRangeMissing, winRangeTooBig)
			}
			if perr != "" {
				return "Error: read failed: " + perr, nil
			}
			out = content
		}
		if isWholeFileRead(params) {
			// Match the native agents: whole-file reads are capped at 51200 chars.
			out = truncateRunes(out, 51200)
		}
	}
	return out, nil
}

// Upload streams a local file to destPath in fixed-size chunks. The control end
// drives the offset loop (read a local chunk -> base64 -> one append command),
// so the shell bot's memory is bounded regardless of file size. The file is
// written to a temporary remote path and renamed into place only after the size
// is verified, so a failed transfer never leaves a partially written destination.

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

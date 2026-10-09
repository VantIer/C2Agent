// Package agent defines the unified controlled-end abstraction: a transport
// layer (Agent) with a per-agent linear job queue, and a Backend interface
// that hides the difference between native protocol agents and reverse-shell
// bots.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kind identifies the controlled-end family.
type Kind string

const (
	KindNative Kind = "native" // binary protocol (remote-c / remote-py / remote-go)
	KindShell  Kind = "shell"  // raw reverse shell
)

// Backend executes high-level actions on one controlled end. Both backends
// must honor ctx cancellation and return *NetworkError / *TimeoutError for
// transport problems.
type Backend interface {
	// Execute runs a high-level action (name matches the LLM tool schema).
	Execute(ctx context.Context, action string, params map[string]any) (string, error)
	// Upload sends a local file to destPath on the controlled end.
	Upload(ctx context.Context, localPath, destPath string) (string, error)
	// Download fetches srcPath into <destDir>/<basename(srcPath)>, overwriting.
	Download(ctx context.Context, srcPath, destDir string) (string, error)
	// Shutdown terminates the controlled-end process / connection.
	Shutdown(ctx context.Context) error
	// Close releases the underlying connection.
	Close() error
}

// BusyReporter is implemented by backends that may still be occupied after an
// execution timeout (a command that is still running on the controlled end).
// Busy reports whether such a timed-out request is still outstanding; while it
// is, the agent must not dispatch further commands.
type BusyReporter interface {
	Busy() bool
}

// NetworkError marks a transport failure (timeout / disconnect / protocol).
type NetworkError struct{ Reason string }

func (e *NetworkError) Error() string { return "network error: " + e.Reason }

// NewNetworkError builds a NetworkError.
func NewNetworkError(format string, args ...any) *NetworkError {
	return &NetworkError{Reason: fmt.Sprintf(format, args...)}
}

// BaseName returns the last path element of p, treating both '/' and '\' as
// separators. The controlled end may run a different OS than the C2, so
// filepath.Base (which only knows the local separator) is not sufficient.
func BaseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// TimeoutError marks an execution that exceeded cmd_timeout.
type TimeoutError struct{ After time.Duration }

func (e *TimeoutError) Error() string { return fmt.Sprintf("execution timed out after %s", e.After) }

// ErrClosed / ErrCancelled are job outcomes when the agent goes away.
// ErrAgentBusy marks a job rejected because the controlled end is still busy
// with an earlier timed-out command.
var (
	ErrClosed    = errors.New("agent closed")
	ErrCancelled = errors.New("job cancelled")
	ErrAgentBusy = errors.New("agent still executing the timed-out command")
)

// Options configures a new Agent.
type Options struct {
	ID            string
	Kind          Kind
	Hostname      string
	OS            string
	Env           string // execution environment (shell bots only): PowerShell/cmd/bash/sh
	Backend       Backend
	QueueCapacity int
	CmdTimeout    time.Duration
	TimeoutAction string // "disconnect" | "fail"
	// OnTimeout is invoked (in the dispatcher goroutine) when an execution
	// times out and TimeoutAction is "disconnect".
	OnTimeout func(*Agent)
}

// Agent is the transport + execution carrier. It owns a linear job queue but
// holds no conversation state (sessions live in the engine).
type Agent struct {
	ID          string
	Kind        Kind
	Hostname    string
	OS          string
	Env         string // shell bots: detected interpreter; native: empty
	ConnectedAt time.Time

	Backend Backend

	activeOps int32
	lastHB    atomic.Int64 // unix nanoseconds

	jobs     chan *Job
	stop     chan struct{}
	stopOnce sync.Once

	cmdTimeout    time.Duration
	timeoutAction string
	onTimeout     func(*Agent)
}

// New creates an Agent and starts its dispatcher goroutine.
func New(o Options) *Agent {
	capacity := o.QueueCapacity
	if capacity <= 0 {
		capacity = 256
	}
	a := &Agent{
		ID:            o.ID,
		Kind:          o.Kind,
		Hostname:      o.Hostname,
		OS:            o.OS,
		Env:           o.Env,
		ConnectedAt:   time.Now(),
		Backend:       o.Backend,
		jobs:          make(chan *Job, capacity),
		stop:          make(chan struct{}),
		cmdTimeout:    o.CmdTimeout,
		timeoutAction: o.TimeoutAction,
		onTimeout:     o.OnTimeout,
	}
	a.TouchHB()
	go a.runDispatcher()
	return a
}

// TouchHB refreshes the heartbeat timestamp.
func (a *Agent) TouchHB() { a.lastHB.Store(time.Now().UnixNano()) }

// LastHB returns the last heartbeat time.
func (a *Agent) LastHB() time.Time { return time.Unix(0, a.lastHB.Load()) }

// SystemName is the target description injected into the system prompt as
// {system_name}. Shell bots report "OS Env" (e.g. "Windows PowerShell") so the
// model knows which command syntax to use; native agents have no interpreter
// and report just the OS.
func (a *Agent) SystemName() string {
	env := a.Env
	if env == "Unknown" {
		env = ""
	}
	switch {
	case a.OS == "" && env == "":
		return "Unknown"
	case env == "":
		return a.OS
	case a.OS == "":
		return env
	default:
		return a.OS + " " + env
	}
}

// ActiveOps reports how many jobs are currently executing.
func (a *Agent) ActiveOps() int32 { return atomic.LoadInt32(&a.activeOps) }

// Stopped reports whether the agent is closed.
func (a *Agent) Stopped() bool {
	select {
	case <-a.stop:
		return true
	default:
		return false
	}
}

// Done returns a channel that is closed when the agent is closed. Callers
// waiting on a job result should select on it so a job enqueued just before
// Close cannot block forever.
func (a *Agent) Done() <-chan struct{} { return a.stop }

// Close stops the dispatcher and closes the backend. Idempotent.
func (a *Agent) Close() {
	a.stopOnce.Do(func() {
		close(a.stop)
		if a.Backend != nil {
			_ = a.Backend.Close()
		}
	})
}

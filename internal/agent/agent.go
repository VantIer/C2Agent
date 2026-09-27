// Package agent defines the unified controlled-end abstraction: a transport
// layer (Agent) with a per-agent linear job queue, and a Backend interface
// that hides the difference between native protocol agents and reverse-shell
// bots.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Kind identifies the controlled-end family.
type Kind string

const (
	KindNative Kind = "native" // binary protocol (remote-c / remote-py)
	KindShell  Kind = "shell"  // raw reverse shell
)

// Backend executes high-level actions on one controlled end. Both backends
// must honor ctx cancellation and return *NetworkError / *TimeoutError for
// transport problems.
type Backend interface {
	Kind() Kind
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

// NetworkError marks a transport failure (timeout / disconnect / protocol).
type NetworkError struct{ Reason string }

func (e *NetworkError) Error() string { return "network error: " + e.Reason }

// NewNetworkError builds a NetworkError.
func NewNetworkError(format string, args ...any) *NetworkError {
	return &NetworkError{Reason: fmt.Sprintf(format, args...)}
}

// TimeoutError marks an execution that exceeded cmd_timeout.
type TimeoutError struct{ After time.Duration }

func (e *TimeoutError) Error() string { return fmt.Sprintf("execution timed out after %s", e.After) }

// ErrClosed / ErrCancelled are job outcomes when the agent goes away.
var (
	ErrClosed    = errors.New("agent closed")
	ErrCancelled = errors.New("job cancelled")
)

// Options configures a new Agent.
type Options struct {
	ID            string
	Kind          Kind
	Hostname      string
	OS            string
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

// Close stops the dispatcher and closes the backend. Idempotent.
func (a *Agent) Close() {
	a.stopOnce.Do(func() { close(a.stop) })
	if a.Backend != nil {
		_ = a.Backend.Close()
	}
}

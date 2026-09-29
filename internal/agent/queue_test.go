package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBackend struct {
	delay   time.Duration
	calls   int32
	onClose int32
}

func (f *fakeBackend) Execute(ctx context.Context, action string, params map[string]any) (string, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "ok:" + action, nil
}
func (f *fakeBackend) Upload(context.Context, string, string) (string, error)   { return "up", nil }
func (f *fakeBackend) Download(context.Context, string, string) (string, error) { return "dl", nil }
func (f *fakeBackend) Shutdown(context.Context) error                           { return nil }
func (f *fakeBackend) Close() error                                             { atomic.AddInt32(&f.onClose, 1); return nil }

func runJob(t *testing.T, a *Agent, action string) (string, error) {
	t.Helper()
	j := NewJob(JobAction)
	j.Action = action
	if err := a.Enqueue(j); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	r := <-j.Result
	return r.Output, r.Err
}

// Queue wait must not count against cmd_timeout: two 80ms jobs with a 100ms
// execution budget both succeed even though the second waits in the queue.
func TestQueueWaitNotTimedOut(t *testing.T) {
	fb := &fakeBackend{delay: 80 * time.Millisecond}
	a := New(Options{ID: "t1", Kind: KindNative, Backend: fb, CmdTimeout: 100 * time.Millisecond, TimeoutAction: "fail"})
	defer a.Close()

	type res struct {
		out string
		err error
	}
	ch := make(chan res, 2)
	for i := 0; i < 2; i++ {
		go func() {
			out, err := runJob(t, a, "list_dir")
			ch <- res{out, err}
		}()
	}
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			t.Fatalf("job %d unexpectedly failed: %v", i, r.err)
		}
		if r.out != "ok:list_dir" {
			t.Fatalf("unexpected output: %q", r.out)
		}
	}
	if got := atomic.LoadInt32(&fb.calls); got != 2 {
		t.Fatalf("expected 2 executions, got %d", got)
	}
}

// An execution exceeding cmd_timeout yields a *TimeoutError.
func TestExecutionTimeout(t *testing.T) {
	fb := &fakeBackend{delay: 300 * time.Millisecond}
	a := New(Options{ID: "t2", Kind: KindNative, Backend: fb, CmdTimeout: 50 * time.Millisecond, TimeoutAction: "fail"})
	defer a.Close()

	_, err := runJob(t, a, "exec_cmd")
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected TimeoutError, got %v", err)
	}
}

// timeout_action=disconnect invokes the callback.
func TestTimeoutDisconnectCallback(t *testing.T) {
	fb := &fakeBackend{delay: 200 * time.Millisecond}
	var fired int32
	a := New(Options{
		ID: "t3", Kind: KindNative, Backend: fb,
		CmdTimeout: 40 * time.Millisecond, TimeoutAction: "disconnect",
		OnTimeout: func(*Agent) { atomic.AddInt32(&fired, 1) },
	})
	defer a.Close()

	_, err := runJob(t, a, "exec_cmd")
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected TimeoutError, got %v", err)
	}
	if atomic.LoadInt32(&fired) != 1 {
		t.Fatalf("OnTimeout callback not invoked")
	}
}

// Enqueue after Close fails fast.
func TestEnqueueAfterClose(t *testing.T) {
	fb := &fakeBackend{}
	a := New(Options{ID: "t4", Kind: KindNative, Backend: fb})
	a.Close()
	j := NewJob(JobAction)
	j.Action = "get_cwd"
	if err := a.Enqueue(j); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

// busyBackend reports Busy after a timed-out execution, like the shell/native
// backends do when the controlled end is still running a command.
type busyBackend struct {
	fakeBackend
	busy int32
}

func (b *busyBackend) Execute(ctx context.Context, action string, params map[string]any) (string, error) {
	atomic.AddInt32(&b.calls, 1)
	select {
	case <-time.After(b.delay):
		return "ok:" + action, nil
	case <-ctx.Done():
		atomic.StoreInt32(&b.busy, 1)
		return "", ctx.Err()
	}
}

func (b *busyBackend) Busy() bool { return atomic.LoadInt32(&b.busy) == 1 }

// With timeout_action=fail, a timed-out command that is still running must
// drain the queued jobs (ErrAgentBusy) instead of letting them time out too.
func TestFailTimeoutDrainsQueueWhenBusy(t *testing.T) {
	bb := &busyBackend{fakeBackend: fakeBackend{delay: 200 * time.Millisecond}}
	a := New(Options{
		ID: "b1", Kind: KindShell, Backend: bb,
		CmdTimeout: 40 * time.Millisecond, TimeoutAction: "fail", QueueCapacity: 16,
	})
	defer a.Close()

	j1 := NewJob(JobAction)
	j1.Action = "exec_cmd"
	if err := a.Enqueue(j1); err != nil {
		t.Fatal(err)
	}
	j2 := NewJob(JobAction)
	j2.Action = "list_dir"
	if err := a.Enqueue(j2); err != nil {
		t.Fatal(err)
	}

	r1 := <-j1.Result
	var te *TimeoutError
	if !errors.As(r1.Err, &te) {
		t.Fatalf("j1: expected TimeoutError, got %v", r1.Err)
	}
	r2 := <-j2.Result
	if !errors.Is(r2.Err, ErrAgentBusy) {
		t.Fatalf("j2: expected ErrAgentBusy, got %v", r2.Err)
	}
}

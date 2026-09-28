package agent

import (
	"context"
	"errors"
	"sync/atomic"
)

// JobType classifies a queued operation.
type JobType int

const (
	JobAction JobType = iota
	JobUpload
	JobDownload
	JobShutdown
	JobDirect
)

// JobResult is the outcome delivered back to the enqueuing session.
type JobResult struct {
	Output string
	Err    error
}

// Job is one unit of work on an Agent's linear queue.
type Job struct {
	Type   JobType
	Action string
	Params map[string]any

	LocalPath   string
	DestPath    string
	SrcPath     string
	DownloadDir string

	Result chan JobResult

	ctx       context.Context
	cancel    context.CancelFunc
	cancelled atomic.Bool
}

// NewJob creates a job with its own cancellable context and result channel.
func NewJob(jt JobType) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	return &Job{
		Type:   jt,
		Result: make(chan JobResult, 1),
		ctx:    ctx,
		cancel: cancel,
	}
}

// Cancel marks the job cancelled and cancels its context.
func (j *Job) Cancel() {
	j.cancelled.Store(true)
	if j.cancel != nil {
		j.cancel()
	}
}

// Cancelled reports whether the job was cancelled.
func (j *Job) Cancelled() bool { return j.cancelled.Load() }

// Enqueue appends a job to the linear queue. Waiting for queue space is not
// subject to any timeout; it only ends when the job is accepted or the agent
// is closed.
func (a *Agent) Enqueue(j *Job) error {
	if j.Result == nil {
		j.Result = make(chan JobResult, 1)
	}
	if j.ctx == nil {
		j.ctx, j.cancel = context.WithCancel(context.Background())
	}
	if a.Stopped() {
		return ErrClosed
	}
	select {
	case a.jobs <- j:
		return nil
	case <-a.stop:
		return ErrClosed
	}
}

func (a *Agent) runDispatcher() {
	for {
		select {
		case <-a.stop:
			a.failPending()
			return
		case j := <-a.jobs:
			if j == nil {
				continue
			}
			a.execJob(j)
		}
	}
}

func (a *Agent) execJob(j *Job) {
	if j.Cancelled() || a.Stopped() {
		j.Result <- JobResult{Err: ErrCancelled}
		return
	}
	atomic.AddInt32(&a.activeOps, 1)
	defer atomic.AddInt32(&a.activeOps, -1)

	ctx := j.ctx
	if a.cmdTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.cmdTimeout)
		defer cancel()
	}

	out, err := a.dispatch(ctx, j)

	if j.Cancelled() {
		// A cancelled job reports uniformly as ErrCancelled, whether it was
		// cancelled before starting or while in flight (its context is
		// cancelled, so dispatch returns context.Canceled).
		err = ErrCancelled
	} else if err != nil && errors.Is(err, context.DeadlineExceeded) {
		// Only reclassify a genuine context timeout; never mask a business
		// error that merely coincided with the deadline.
		err = &TimeoutError{After: a.cmdTimeout}
		if a.timeoutAction == "disconnect" && a.onTimeout != nil {
			a.onTimeout(a)
		}
	}
	j.Result <- JobResult{Output: out, Err: err}
}

func (a *Agent) dispatch(ctx context.Context, j *Job) (string, error) {
	switch j.Type {
	case JobUpload:
		return a.Backend.Upload(ctx, j.LocalPath, j.DestPath)
	case JobDownload:
		return a.Backend.Download(ctx, j.SrcPath, j.DownloadDir)
	case JobShutdown:
		return "", a.Backend.Shutdown(ctx)
	default:
		return a.Backend.Execute(ctx, j.Action, j.Params)
	}
}

func (a *Agent) failPending() {
	for {
		select {
		case j := <-a.jobs:
			if j != nil {
				j.Result <- JobResult{Err: ErrClosed}
			}
		default:
			return
		}
	}
}

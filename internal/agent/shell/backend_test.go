package shell

import (
	"net"
	"testing"
)

func TestMarkDrainingStopsBufferingAndClearsLines(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	b := newBackend("t", c1)
	p := &pendingReq{marker: "__C2AGENT_abc__", ch: make(chan shellResult, 1), lines: []string{"old"}}
	b.pending = p

	b.markDraining(p)
	if len(p.lines) != 0 {
		t.Fatalf("draining should clear buffered lines, got %v", p.lines)
	}
	if !p.draining {
		t.Fatal("draining flag not set")
	}
	if !b.Busy() {
		t.Fatal("Busy should be true while a request is pending")
	}
}

// While draining, output is not buffered; the marker still clears pending.
func TestHandleDataDiscardsWhileDrainingAndClearsOnMarker(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	b := newBackend("t", c1)
	marker := "__C2AGENT_abc__"
	p := &pendingReq{marker: marker, ch: make(chan shellResult, 1)}
	b.pending = p
	b.markDraining(p)

	b.handleData("line1\nline2\n" + marker + "\n")

	if b.Busy() {
		t.Fatal("Busy should be false after the marker arrives")
	}
	if len(p.lines) != 0 {
		t.Fatalf("draining must not buffer content, got %v", p.lines)
	}
}

// A normal (non-draining) request still buffers content and is not Busy once
// its marker arrives.
func TestHandleDataBuffersNormally(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	b := newBackend("t", c1)
	marker := "__C2AGENT_abc__"
	p := &pendingReq{marker: marker, ch: make(chan shellResult, 1)}
	b.pending = p

	b.handleData("hello\n" + marker + "\n")

	if b.Busy() {
		t.Fatal("Busy should be false after the marker arrives")
	}
}

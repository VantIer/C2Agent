package native

import (
	"net"
	"testing"

	"c2agent/internal/protocol"
)

// Busy tracks timed-out requests until their terminal packet arrives.
func TestBusyTracksLateRequests(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	b := NewBackend(c1)
	if b.Busy() {
		t.Fatal("a fresh backend must not be busy")
	}

	b.markLate(42)
	if !b.Busy() {
		t.Fatal("should be busy after a request is marked late")
	}

	// A "continue" data packet must NOT clear the late state.
	b.deliver(&protocol.Packet{ReqID: 42, Cmd: protocol.EndFlagContinue, Body: []byte("x")})
	if !b.Busy() {
		t.Fatal("a continue data packet must not clear late")
	}

	// The terminal response does clear it.
	b.deliver(&protocol.Packet{ReqID: 42, Cmd: protocol.CmdListDir, Body: []byte("ok")})
	if b.Busy() {
		t.Fatal("a terminal packet should clear late")
	}
}

// A data "last" packet also completes the request.
func TestBusyClearedByLastDataPacket(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	b := NewBackend(c1)
	b.markLate(7)
	b.deliver(&protocol.Packet{ReqID: 7, Cmd: protocol.EndFlagLast, Body: nil})
	if b.Busy() {
		t.Fatal("an end data packet should clear late")
	}
}

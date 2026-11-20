package main

import (
	"testing"
	"time"
)

func TestApplyRegisterAck(t *testing.T) {
	rc := newRuntimeConfig()
	body := encodeTLV([]string{"40", "15"})
	if err := applyRegisterAck(body, rc); err != nil {
		t.Fatalf("applyRegisterAck: %v", err)
	}
	if rc.heartbeatInterval() != 40*time.Second || rc.commandTimeout() != 15*time.Second {
		t.Fatalf("ACK not applied: %v / %v", rc.heartbeatInterval(), rc.commandTimeout())
	}
	if err := applyRegisterAck(encodeTLV([]string{"40"}), rc); err == nil {
		t.Fatal("expected error for a short ACK payload")
	}
	if err := applyRegisterAck(encodeTLV([]string{"0", "15"}), rc); err == nil {
		t.Fatal("expected error for a non-positive heartbeat")
	}
	if err := applyRegisterAck(encodeTLV([]string{"601", "15"}), rc); err == nil {
		t.Fatal("expected error for a heartbeat above the maximum")
	}
	if err := applyRegisterAck(encodeTLV([]string{"40", "601"}), rc); err == nil {
		t.Fatal("expected error for a command timeout above the maximum")
	}
}

func TestRuntimeConfigUpdate(t *testing.T) {
	rc := newRuntimeConfig()
	if rc.heartbeatInterval() != 30*time.Second || rc.commandTimeout() != 60*time.Second {
		t.Fatalf("unexpected defaults: %v / %v", rc.heartbeatInterval(), rc.commandTimeout())
	}

	rc.set(40, 15)
	if rc.heartbeatInterval() != 40*time.Second || rc.commandTimeout() != 15*time.Second {
		t.Fatalf("update not applied: %v / %v", rc.heartbeatInterval(), rc.commandTimeout())
	}

	// Non-positive values must not clobber the current settings.
	rc.set(0, -1)
	if rc.heartbeatInterval() != 40*time.Second || rc.commandTimeout() != 15*time.Second {
		t.Fatalf("invalid update mutated state: %v / %v", rc.heartbeatInterval(), rc.commandTimeout())
	}
}

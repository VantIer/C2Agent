package main

import (
	"sync"
	"unicode/utf8"
)

const (
	maxOutputBytes      = 8 << 20
	maxReadFileLines    = 512
	outputTruncatedText = "\n[... output truncated at 8 MiB]"
)

// capOutput bounds an action result and appends a clear truncation marker.
// truncated reports that the producer already discarded bytes (for example a
// capped command-output writer).
func capOutput(s string, truncated bool) string {
	if len(s) <= maxOutputBytes && !truncated {
		return s
	}
	limit := maxOutputBytes - len(outputTruncatedText)
	if limit < 0 {
		limit = 0
	}
	if limit > len(s) {
		limit = len(s)
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + outputTruncatedText
}

// limitedBuffer keeps at most limit bytes while accepting (and discarding)
// everything after the limit, so a child process can never block on a full
// pipe just because its output is being capped.
type limitedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) < b.limit {
		take := b.limit - len(b.buf)
		if take > len(p) {
			take = len(p)
		}
		b.buf = append(b.buf, p[:take]...)
		if take < len(p) {
			b.truncated = true
		}
	} else {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf
}

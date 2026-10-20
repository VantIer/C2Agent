// Package limits holds cross-package resource limits that must stay consistent
// between the control end and the shell backend.
package limits

const (
	// MaxOutputBytes bounds one action/command result (including the marker).
	MaxOutputBytes = 8 << 20

	// OutputTruncatedMarker is appended when output is truncated.
	OutputTruncatedMarker = "\n[... output truncated at 8 MiB]"

	// ReadFileLines bounds a read_file line range.
	ReadFileLines = 512
)

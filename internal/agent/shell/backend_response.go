package shell

import "strings"

// extractWrappedText returns the payload framed by begin/end, ignoring any
// surrounding stream noise. A lone `missing` line is reported as an error.
func extractWrappedText(out, begin, end, missing string) (string, string) {
	lines := strings.Split(out, "\n")
	for _, l := range lines {
		if strings.TrimSpace(l) == missing {
			return "", "file not found, not readable, or not a regular file"
		}
	}
	bi := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == begin {
			bi = i
			break
		}
	}
	if bi < 0 {
		return "", "unexpected response while reading file"
	}
	ei := -1
	for i := bi + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == end {
			ei = i
			break
		}
	}
	if ei < 0 {
		return "", "unexpected response while reading file"
	}
	return strings.TrimSpace(strings.Join(lines[bi+1:ei], "\n")), ""
}

// extractWrappedRange parses a sentinel-wrapped line-range read. RANGE means
// the start line is past EOF; TOOBIG means the selected bytes exceed the cap.
func extractWrappedRange(out, begin, end, missing, rangeMissing, tooBig string) (string, string) {
	for _, l := range strings.Split(out, "\n") {
		switch strings.TrimSpace(l) {
		case rangeMissing:
			return "", "start line exceeds file line count"
		case tooBig:
			return "", "selected line range exceeds the shell read limit"
		}
	}
	return extractWrappedEdit(out, begin, end, missing)
}

// extractWrappedEdit pulls the base64 payload out of a sentinel-wrapped read,
// returning the decoded content and an error message ("" on success). Because
// the payload is framed and verified by base64 decoding, file content that
// happens to look like an error message is never misread as a read failure.
func extractWrappedEdit(out, begin, end, missing string) (string, string) {
	payload, perr := extractWrappedText(out, begin, end, missing)
	if perr != "" {
		return "", perr
	}
	data, ok := decodeBase64(payload)
	if !ok {
		return "", "could not decode file content"
	}
	return string(data), ""
}

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

func readFile(path, startLine, endLine string) string {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "File does not exist: " + path
		}
		return "Error reading file: " + err.Error()
	}
	if info.IsDir() {
		return path + " is a directory"
	}
	if startLine == "" || startLine == "0" {
		return readWholeLimited(path)
	}
	s, err := strconv.Atoi(strings.TrimSpace(startLine))
	if err != nil {
		return fmt.Sprintf("Invalid line numbers: start_line=%s, end_line=%s", startLine, endLine)
	}
	start := s - 1
	if start < 0 {
		start = 0
	}
	end := 0 // 0 = to end of file
	if endLine != "" && endLine != "0" {
		if e, err := strconv.Atoi(strings.TrimSpace(endLine)); err == nil {
			end = e
		}
	}
	// A line range is capped at 512 lines, matching the C2-side clamp. An
	// invalid end value must not fall back to end-of-file.
	if end == 0 || end < start+1 || end > start+maxReadFileLines {
		end = start + maxReadFileLines
	}

	f, err := os.Open(path)
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	defer f.Close()

	// Stream line by line, keeping only the requested range, so a huge file is
	// never loaded into memory in full.
	r := bufio.NewReader(f)
	var b strings.Builder
	lineNo := 0
	reachedStart := false
	truncated := false
	maxContent := maxOutputBytes - len(outputTruncatedText)
	for {
		line, rerr := r.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			if lineNo > start {
				reachedStart = true
				if end > 0 && lineNo > end {
					break
				}
				if b.Len()+len(line) > maxContent {
					truncated = true
					break
				}
				b.WriteString(line)
			}
		}
		if rerr != nil {
			break
		}
	}
	if !reachedStart {
		return fmt.Sprintf("Start line %s exceeds file line count (%d)", startLine, lineNo)
	}
	if truncated {
		b.WriteString(outputTruncatedText)
	}
	return b.String()
}

// readWholeLimited reads at most enough bytes to cover readFileLimit characters
// (4 bytes per character in the worst case), so an enormous file cannot exhaust
// memory, then truncates to readFileLimit characters (not bytes).
func readWholeLimited(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(readFileLimit)*4+4))
	if err != nil {
		return "Error reading file: " + err.Error()
	}
	s := string(data)
	if utf8.RuneCountInString(s) <= readFileLimit {
		return s
	}
	n := 0
	for i := range s {
		if n == readFileLimit {
			return s[:i]
		}
		n++
	}
	return s
}

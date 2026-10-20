package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

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
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	f.Flush()
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

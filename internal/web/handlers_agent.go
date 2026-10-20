package web

import "net/http"

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
			"os": a.OS, "env": a.Env, "system_name": a.SystemName(),
			"connected_at": a.ConnectedAt.Unix(),
			"active":       a.ID == reg.ActiveID(),
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

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go"

	"c2agent/internal/agent"
	"c2agent/internal/config"
	"c2agent/internal/engine"
	"c2agent/internal/llm"
)

type fakeLLM struct{}

func (fakeLLM) Chat(context.Context, []llm.Message, []openai.ChatCompletionToolParam, func(string)) (*llm.ChatResult, error) {
	return &llm.ChatResult{Content: "ok"}, nil
}

type fakeBackend struct{}

func (fakeBackend) Execute(_ context.Context, action string, _ map[string]any) (string, error) {
	if action == "list_dir" {
		return "DIR 0 docs\nFILE 12 a.txt", nil
	}
	return "ok", nil
}
func (fakeBackend) Upload(context.Context, string, string) (string, error) { return "uploaded", nil }
func (fakeBackend) Download(context.Context, string, string) (string, error) {
	return "downloaded", nil
}
func (fakeBackend) Shutdown(context.Context) error { return nil }
func (fakeBackend) Close() error                   { return nil }

func newTestServer(t *testing.T) (*Server, *agent.Agent) {
	t.Helper()
	cfg := config.Default()
	reg := agent.NewRegistry()
	ag := agent.New(agent.Options{
		ID: "a1", Kind: agent.KindShell, Hostname: "host", OS: "Linux",
		Backend: fakeBackend{}, CmdTimeout: 5_000_000_000, TimeoutAction: "fail",
	})
	reg.Register(ag)
	t.Cleanup(ag.Close)
	eng := engine.NewWithClient(cfg, reg, fakeLLM{})
	return New(cfg, eng), ag
}

func doRequest(t *testing.T, h http.Handler, method, target string, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestOriginGuard(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.srv.Handler

	rec := doRequest(t, h, http.MethodGet, "/", "", func(r *http.Request) {
		r.Host = "127.0.0.1:8880"
		r.Header.Set("Origin", "http://evil.example")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", rec.Code)
	}

	rec = doRequest(t, h, http.MethodGet, "/", "", func(r *http.Request) {
		r.Host = "127.0.0.1:8880"
		r.Header.Set("Origin", "http://127.0.0.1:8880")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin status = %d, want 200", rec.Code)
	}
}

func TestAgentsEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doRequest(t, srv.srv.Handler, http.MethodGet, "/api/agents", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Agents []struct {
			ID string `json:"id"`
		} `json:"agents"`
		Active string `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Agents) != 1 || body.Agents[0].ID != "a1" || body.Active != "a1" {
		t.Fatalf("unexpected agents response: %s", rec.Body.String())
	}
}

func TestSessionEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.srv.Handler

	rec := doRequest(t, h, http.MethodPost, "/api/sessions/new",
		`{"agent_id":"a1","title":"review"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("new session status = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Session == "" {
		t.Fatalf("bad new session response: %s", rec.Body.String())
	}

	rec = doRequest(t, h, http.MethodGet, "/api/sessions?agent_id=a1", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.Session) {
		t.Fatalf("session list missing session: %s", rec.Body.String())
	}

	rec = doRequest(t, h, http.MethodGet, "/api/history?session_id="+created.Session, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d", rec.Code)
	}
}

func TestFileListEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doRequest(t, srv.srv.Handler, http.MethodGet, "/api/files/list?agent_id=a1&path=.", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
			Size  int64  `json:"size"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 2 || body.Items[0].Name != "docs" || !body.Items[0].IsDir || body.Items[1].Size != 12 {
		t.Fatalf("unexpected listing: %s", rec.Body.String())
	}
}

func TestUnknownSessionStream(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doRequest(t, srv.srv.Handler, http.MethodGet, "/api/chat-stream?session_id=missing", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestExecAndFileActions(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.srv.Handler

	cases := []struct {
		method, target, body string
		want                 int
	}{
		{http.MethodPost, "/api/exec-cmd", `{"agent_id":"a1","command":"x"}`, 200},
		{http.MethodGet, "/api/files/cwd?agent_id=a1", "", 200},
		{http.MethodPost, "/api/files/mkdir", `{"agent_id":"a1","path":"/x"}`, 200},
		{http.MethodPost, "/api/files/new", `{"agent_id":"a1","path":"/x"}`, 200},
		{http.MethodPost, "/api/files/delete", `{"agent_id":"a1","path":"/x"}`, 200},
		{http.MethodPost, "/api/files/copy", `{"agent_id":"a1","src":"/a","dest":"/b"}`, 200},
		{http.MethodPost, "/api/files/move", `{"agent_id":"a1","src":"/a","dest":"/b"}`, 200},
		{http.MethodPost, "/api/shutdown", `{"agent_id":"a1"}`, 200},
	}
	for _, tc := range cases {
		rec := doRequest(t, h, tc.method, tc.target, tc.body, nil)
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d: %s", tc.method, tc.target, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestAuthAndSessionControl(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.srv.Handler

	rec := doRequest(t, h, http.MethodPost, "/api/set-auth", `{"mode":2}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"auth_mode":2`) {
		t.Fatalf("set-auth: %s", rec.Body.String())
	}

	rec = doRequest(t, h, http.MethodPost, "/api/sessions/new", `{"agent_id":"a1"}`, nil)
	var created struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Session == "" {
		t.Fatalf("new session: %s", rec.Body.String())
	}
	session := created.Session

	for _, ep := range []string{"/api/stop", "/api/reset"} {
		body := `{"session_id":"` + session + `"}`
		rec = doRequest(t, h, http.MethodPost, ep, body, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d: %s", ep, rec.Code, rec.Body.String())
		}
	}
	rec = doRequest(t, h, http.MethodPost, "/api/authorize-execute", `{"session_id":"`+session+`","authorized":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("authorize = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, h, http.MethodPost, "/api/send", `{"session_id":"`+session+`","message":"hi"}`, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("send = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestParseListing(t *testing.T) {
	items := parseListing("DIR 0 docs\nFILE 12 a.txt\nnot a listing")
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}
	if items[0]["name"] != "docs" || items[0]["is_dir"] != true {
		t.Fatalf("bad dir item: %#v", items[0])
	}
	if items[1]["size"] != int64(12) {
		t.Fatalf("bad file size: %#v", items[1])
	}
}

func TestListingError(t *testing.T) {
	if _, ok := listingError("Error: access denied"); !ok {
		t.Fatal("expected error listing")
	}
	if _, ok := listingError("DIR 0 docs"); ok {
		t.Fatal("listing should not be an error")
	}
}

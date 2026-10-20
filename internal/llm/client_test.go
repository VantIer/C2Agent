package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sseServer(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, c := range chunks {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", c); err != nil {
				return
			}
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
}

func TestChatStreamContent(t *testing.T) {
	srv := sseServer(t,
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hel"},"finish_reason":null}]}`,
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	defer srv.Close()

	c := New(srv.URL, "key", "model", 0, true)
	var got strings.Builder
	res, err := c.Chat(context.Background(), []Message{UserMessage("hi")}, nil, func(chunk string) {
		got.WriteString(chunk)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "hello" || res.Content != "hello" {
		t.Fatalf("streamed %q, result %q", got.String(), res.Content)
	}
}

func TestChatStreamToolCalls(t *testing.T) {
	srv := sseServer(t,
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"list_dir","arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\".\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)
	defer srv.Close()

	c := New(srv.URL, "key", "model", 0, true)
	res, err := c.Chat(context.Background(), []Message{UserMessage("hi")}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", res.ToolCalls)
	}
	tc := res.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "list_dir" || tc.Arguments != `{"path":"."}` {
		t.Fatalf("unexpected tool call: %#v", tc)
	}
}

func TestBuildTools(t *testing.T) {
	tools := BuildTools()
	if len(tools) == 0 {
		t.Fatal("no tools generated")
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Function.Name] = true
	}
	for _, want := range []string{"read_file", "list_dir", "exec_cmd"} {
		if !names[want] {
			t.Fatalf("tool %q missing", want)
		}
	}
}

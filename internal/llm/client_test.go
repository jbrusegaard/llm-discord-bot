package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer returns a fake /v1 server that records the last request body
// and replies with an assistant message built from the given JSON fragment.
func newTestServer(t *testing.T, replyFragment string) (*httptest.Server, *string) {
	t.Helper()
	var lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		lastBody = string(data)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":%s}]}`, replyFragment)
	}))
	t.Cleanup(srv.Close)
	return srv, &lastBody
}

func TestChat(t *testing.T) {
	srv, body := newTestServer(t, `{"role":"assistant","content":"hello there"}`)
	c := New(srv.URL, "", "test-model")
	got, err := c.Chat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got != "hello there" {
		t.Fatalf("Chat = %q, want greeting", got)
	}

	var req ChatRequest
	if err := json.Unmarshal([]byte(*body), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if req.Model != "test-model" || len(req.Messages) != 1 {
		t.Fatalf("request = %+v, want model + one message", req)
	}
	if strings.Contains(*body, `"tools"`) {
		t.Fatalf("plain Chat should not send tools: %s", *body)
	}
}

func TestChatWithTools(t *testing.T) {
	reply := `{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"create_reminder","arguments":"{\"message\":\"stretch\",\"delay_minutes\":10}"}}]}`
	srv, body := newTestServer(t, reply)
	c := New(srv.URL, "", "test-model")

	tool := Tool{Type: "function", Function: Function{Name: "create_reminder"}}
	msg, err := c.ChatWithTools(context.Background(), []Message{{Role: RoleUser, Content: "remind me in 10 minutes to stretch"}}, []Tool{tool})
	if err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}

	// The tools schema must be sent in the request.
	var req ChatRequest
	if err := json.Unmarshal([]byte(*body), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "create_reminder" {
		t.Fatalf("request tools = %+v, want create_reminder offered", req.Tools)
	}

	// The tool call must be parsed out of the response.
	if msg.Content != "" {
		t.Fatalf("tool-call reply should have empty content, got %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "create_reminder" {
		t.Fatalf("tool call = %+v, want create_reminder/call_1", tc)
	}
	var args struct {
		Message      string  `json:"message"`
		DelayMinutes float64 `json:"delay_minutes"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("decode arguments: %v", err)
	}
	if args.Message != "stretch" || args.DelayMinutes != 10 {
		t.Fatalf("arguments = %+v, want stretch/10", args)
	}
}

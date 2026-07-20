package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSelectAgent(t *testing.T) {
	agents := []cliAgent{
		{ID: "agt_1", Name: "Coder"},
		{ID: "agt_2", Name: "Researcher"},
	}

	got, err := selectAgent(agents, "coder")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "agt_1" {
		t.Fatalf("selected %q, want agt_1", got.ID)
	}

	if _, err := selectAgent(agents, "missing"); err == nil {
		t.Fatal("expected missing agent error")
	}
}

func TestSelectAgentDefaultsToFirstCreated(t *testing.T) {
	first := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	latest := first.Add(time.Hour)
	agents := []cliAgent{
		{ID: "agt_latest", Name: "Latest", CreatedAt: latest},
		{ID: "agt_first", Name: "First", CreatedAt: first},
	}

	got, err := selectAgent(agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "agt_first" {
		t.Fatalf("selected %q, want first-created agent", got.ID)
	}
}

func TestRenderTerminalMarkdown(t *testing.T) {
	got, err := renderTerminalMarkdown("**bold**\n\n- one\n- two\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "**bold**") {
		t.Fatalf("markdown syntax was not rendered: %q", got)
	}
	if !strings.Contains(got, "bold") || !strings.Contains(got, "one") {
		t.Fatalf("rendered output lost content: %q", got)
	}
}

func TestCompleteMarkdownPrefix(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "completed paragraph",
			text: "**bold** paragraph\n\nunfinished",
			want: "**bold** paragraph\n\n",
		},
		{
			name: "unfinished paragraph",
			text: "still arriving",
			want: "",
		},
		{
			name: "blank line inside fence",
			text: "```go\nfmt.Println(1)\n\nmore",
			want: "",
		},
		{
			name: "closed fence",
			text: "```go\nfmt.Println(1)\n\n```\nnext",
			want: "```go\nfmt.Println(1)\n\n```\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cut := completeMarkdownPrefix(tt.text)
			if got := tt.text[:cut]; got != tt.want {
				t.Fatalf("complete prefix = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChatClientStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"content_delta\",\"data\":{\"delta\":\"Hello\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"content_delta\",\"data\":{\"delta\":\" world\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"content\",\"data\":{\"content\":\"Hello world\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\"}\n\n"))
	}))
	defer server.Close()

	c := &chatClient{baseURL: server.URL, apiKey: "test-token", http: server.Client()}
	var out bytes.Buffer
	if err := c.stream(context.Background(), "agt_1", "session-1", "hi", &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "Hello world\n" {
		t.Fatalf("output = %q", got)
	}
}

type observingWriter struct {
	mu     sync.Mutex
	writes []string
}

func (w *observingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, string(p))
	w.mu.Unlock()
	return len(p), nil
}

func TestChatClientStreamWritesDeltasAndToolProgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"content_delta\",\"data\":{\"delta\":\"first\"}}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"tool_call\",\"data\":{\"id\":\"call-1\",\"name\":\"exec\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"tool_result\",\"data\":{\"id\":\"call-1\",\"result\":\"command completed successfully\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"done\"}\n\n"))
	}))
	defer server.Close()

	out := &observingWriter{}
	c := &chatClient{baseURL: server.URL, apiKey: "test-token", http: server.Client()}
	if err := c.stream(context.Background(), "agt_1", "session-1", "hi", out); err != nil {
		t.Fatal(err)
	}
	out.mu.Lock()
	writes := append([]string(nil), out.writes...)
	out.mu.Unlock()
	if len(writes) < 4 || writes[0] != "first" {
		t.Fatalf("expected first delta to be its own immediate write, got %#v", writes)
	}
	joined := strings.Join(writes, "")
	if !strings.Contains(joined, "↳ exec") || !strings.Contains(joined, "✓ exec command completed successfully") {
		t.Fatalf("tool progress missing from %q", joined)
	}
	if strings.Contains(joined, "\x1b[") {
		t.Fatalf("non-terminal output contains ANSI escapes: %q", joined)
	}
}

func TestNewCLISessionID(t *testing.T) {
	a := newCLISessionID()
	b := newCLISessionID()
	if !strings.HasPrefix(a, "cli-") {
		t.Fatalf("session ID %q lacks cli prefix", a)
	}
	if a == b {
		t.Fatalf("session IDs unexpectedly equal: %q", a)
	}
}

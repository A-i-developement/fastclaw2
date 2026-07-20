package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/cliclient"
)

type fakeClient struct{}

func (fakeClient) Agents(context.Context) ([]cliclient.Agent, error) { return nil, nil }
func (fakeClient) Sessions(context.Context, string) ([]cliclient.Session, error) {
	return nil, nil
}
func (fakeClient) History(context.Context, string, string) ([]cliclient.HistoryMessage, error) {
	return nil, nil
}
func (fakeClient) Stream(context.Context, string, string, string, func(cliclient.Event)) error {
	return nil
}
func (fakeClient) Steer(context.Context, string, string, string) (bool, error) { return false, nil }
func (fakeClient) RenameSession(context.Context, string, string) error         { return nil }
func (fakeClient) BaseURL() string                                             { return "http://127.0.0.1:18953" }

func newTestModel() *Model {
	m := NewModel(Options{
		Client:    fakeClient{},
		Agent:     cliclient.Agent{ID: "agt_1", Name: "Coder", Model: "claude"},
		SessionID: "cli-test",
	})
	m.width, m.height, m.ready = 100, 40, true
	return m
}

func evt(typ string, kv ...string) cliclient.Event {
	data := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		data[kv[i]] = kv[i+1]
	}
	return cliclient.Event{Type: typ, Data: data}
}

func TestStreamEventFolding(t *testing.T) {
	m := newTestModel()
	m.querying = true

	m.handleStreamEvent(evt("content_delta", "delta", "让我看一下"))
	m.handleStreamEvent(evt("tool_call", "id", "c1", "name", "exec_command"))
	m.handleStreamEvent(evt("tool_result", "id", "c1", "result", "ok"))
	m.handleStreamEvent(evt("content_delta", "delta", "结论：没问题"))
	m.handleStreamEvent(evt("content", "content", "should be ignored, deltas streamed"))

	// delta → assistant block, tool block, then final assistant block
	if len(m.blocks) != 3 {
		t.Fatalf("blocks = %d, want 3: %#v", len(m.blocks), m.blocks)
	}
	if m.blocks[0].Kind != blockAssistant || m.blocks[0].Content != "让我看一下" {
		t.Fatalf("first block = %+v", m.blocks[0])
	}
	if m.blocks[1].Kind != blockTool || len(m.blocks[1].Tools) != 1 {
		t.Fatalf("tool block = %+v", m.blocks[1])
	}
	tool := m.blocks[1].Tools[0]
	if tool.Name != "exec_command" || !tool.Done || tool.Summary != "ok" {
		t.Fatalf("tool state = %+v", tool)
	}
	if m.blocks[2].Content != "结论：没问题" {
		t.Fatalf("final block = %+v", m.blocks[2])
	}
}

func TestStreamContentWithoutDeltas(t *testing.T) {
	m := newTestModel()
	m.querying = true
	m.handleStreamEvent(evt("content", "content", "整段返回"))
	if len(m.blocks) != 1 || m.blocks[0].Content != "整段返回" {
		t.Fatalf("blocks = %#v", m.blocks)
	}
}

func TestConsecutiveToolCallsShareBlock(t *testing.T) {
	m := newTestModel()
	m.querying = true
	m.handleStreamEvent(evt("tool_call", "id", "c1", "name", "read_file"))
	m.handleStreamEvent(evt("tool_call", "id", "c2", "name", "write_file"))
	if len(m.blocks) != 1 || len(m.blocks[0].Tools) != 2 {
		t.Fatalf("expected one tool block with two tools, got %#v", m.blocks)
	}
}

func TestMatchSlashCommands(t *testing.T) {
	if got := matchSlashCommands("/se"); len(got) != 1 || got[0].Name != "/sessions" {
		t.Fatalf("matches for /se = %#v", got)
	}
	// Alias prefix should surface the canonical command.
	if got := matchSlashCommands("/res"); len(got) != 1 || got[0].Name != "/sessions" {
		t.Fatalf("matches for /res = %#v", got)
	}
	if got := matchSlashCommands("hello"); got != nil {
		t.Fatalf("non-slash input matched %#v", got)
	}
}

func TestCanonicalSlash(t *testing.T) {
	name, args := canonicalSlash("/rename 我的会话")
	if name != "/rename" || args != "我的会话" {
		t.Fatalf("canonicalSlash = %q %q", name, args)
	}
	if name, _ := canonicalSlash("/quit"); name != "/exit" {
		t.Fatalf("alias /quit resolved to %q", name)
	}
	if name, _ := canonicalSlash("/nonsense"); name != "" {
		t.Fatalf("unknown command resolved to %q", name)
	}
}

func TestPickerFilterAndSelect(t *testing.T) {
	p := newPicker("test", []pickerItem{
		{ID: "a", Title: "修复登录问题"},
		{ID: "b", Title: "部署 sandbox"},
	}, 100)

	if chosen, done := p.Handle("s"); chosen != nil || done {
		t.Fatal("typing should not close the picker")
	}
	// "s" matches "sandbox" only (filter is case-insensitive substring).
	items := p.filtered()
	if len(items) != 1 || items[0].ID != "b" {
		t.Fatalf("filtered = %#v", items)
	}
	chosen, done := p.Handle("enter")
	if !done || chosen == nil || chosen.ID != "b" {
		t.Fatalf("enter selection = %v %v", chosen, done)
	}
}

func TestHelpTextListsCommands(t *testing.T) {
	help := helpText()
	for _, want := range []string{"/new", "/sessions", "/agents", "Shift+Enter"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help text missing %q:\n%s", want, help)
		}
	}
}

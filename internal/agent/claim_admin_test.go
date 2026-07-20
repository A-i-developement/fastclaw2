package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

func claimMsg(mut ...func(*bus.InboundMessage)) bus.InboundMessage {
	msg := bus.InboundMessage{
		Channel:  "telegram",
		ChatID:   "chat-1",
		UserID:   "10001",
		PeerKind: "dm",
		Source:   bus.SourceUser,
	}
	for _, m := range mut {
		m(&msg)
	}
	return msg
}

// First DM on a fresh channel claims admin and persists to agent.json
// (file mode: dataStore nil). A later, different DM user must NOT claim.
func TestFirstDMClaimsChannelAdmin(t *testing.T) {
	home := t.TempDir()
	a := &Agent{name: "agt_test", homePath: home}

	a.maybeClaimChannelAdmin(context.Background(), claimMsg())
	if got := a.admins["telegram"]; len(got) != 1 || got[0] != "10001" {
		t.Fatalf("admins[telegram] = %v, want [10001]", got)
	}

	data, err := os.ReadFile(filepath.Join(home, "agent.json"))
	if err != nil {
		t.Fatalf("agent.json not written: %v", err)
	}
	var cfg struct {
		Admins map[string][]string `json:"admins"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("agent.json parse: %v", err)
	}
	if got := cfg.Admins["telegram"]; len(got) != 1 || got[0] != "10001" {
		t.Fatalf("persisted admins = %v, want [10001]", cfg.Admins)
	}

	a.maybeClaimChannelAdmin(context.Background(), claimMsg(func(m *bus.InboundMessage) {
		m.UserID = "99999"
		m.ChatID = "chat-2"
	}))
	if got := a.admins["telegram"]; len(got) != 1 || got[0] != "10001" {
		t.Fatalf("second DM user overwrote claim: %v", got)
	}
}

// The claim must never fire from groups, runtime-originated turns,
// web/api channels, hosted deploys, or channels with an explicit list.
func TestClaimGuards(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*bus.InboundMessage)
		prep func(*Agent)
		env  bool // set FASTCLAW_DEPLOY=hosted
	}{
		{name: "group chat", mut: func(m *bus.InboundMessage) { m.PeerKind = "group" }},
		{name: "cron replay", mut: func(m *bus.InboundMessage) { m.Source = bus.SourceCron }},
		{name: "subagent", mut: func(m *bus.InboundMessage) { m.Source = bus.SourceSubAgent }},
		{name: "web channel", mut: func(m *bus.InboundMessage) { m.Channel = "web" }},
		{name: "empty user", mut: func(m *bus.InboundMessage) { m.UserID = "" }},
		{name: "hosted deploy", mut: func(m *bus.InboundMessage) {}, env: true},
		{name: "explicit list wins", mut: func(m *bus.InboundMessage) {}, prep: func(a *Agent) {
			a.admins = map[string][]string{"telegram": {"someone-else"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env {
				t.Setenv("FASTCLAW_DEPLOY", "hosted")
			}
			a := &Agent{name: "agt_test", homePath: t.TempDir()}
			if tc.prep != nil {
				tc.prep(a)
			}
			a.maybeClaimChannelAdmin(context.Background(), claimMsg(tc.mut))
			if got := a.admins["telegram"]; len(got) == 1 && got[0] == "10001" {
				t.Fatalf("%s: claim fired, want blocked", tc.name)
			}
		})
	}
}

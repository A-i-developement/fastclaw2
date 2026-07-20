package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/sandbox"
)

// recordingExecutor captures the last command Exec received so tests can
// assert the sandbox path was (or wasn't) taken.
type recordingExecutor struct {
	fakeExecutor
	lastCommand string
}

func (r *recordingExecutor) Exec(_ context.Context, command string, _ time.Duration) (string, error) {
	r.lastCommand = command
	return "sandbox-ran", nil
}

// In optional-sandbox mode (self-hosted default: no sbCfg, sandboxRequired
// false, only a lazy provider wired), plain exec must run on the host and
// exec(sandbox:true) must route through the provider's executor.
func TestOptionalSandboxExecRouting(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	registerExecFull(r, nil, nil, nil)

	ex := &recordingExecutor{}
	r.SetSandboxProvider(func(context.Context) (sandbox.Executor, error) { return ex, nil })

	out, err := r.Execute(context.Background(), "exec", `{"command":"echo host-ran"}`)
	if err != nil {
		t.Fatalf("host exec: %v", err)
	}
	if !strings.Contains(out, "host-ran") {
		t.Fatalf("host exec output = %q, want host shell echo", out)
	}
	if ex.lastCommand != "" {
		t.Fatalf("host exec leaked into sandbox executor: %q", ex.lastCommand)
	}

	out, err = r.Execute(context.Background(), "exec", `{"command":"echo in-sandbox","sandbox":true}`)
	if err != nil {
		t.Fatalf("sandbox exec: %v", err)
	}
	if !strings.HasPrefix(out, MetaSandboxPrefix) || !strings.Contains(out, "sandbox-ran") {
		t.Fatalf("sandbox exec output = %q, want MetaSandboxPrefix + executor result", out)
	}
	if ex.lastCommand != "echo in-sandbox" {
		t.Fatalf("sandbox executor got command %q, want %q", ex.lastCommand, "echo in-sandbox")
	}
}

// Without a provider (sandbox not configured at all), sandbox:true must
// refuse loudly instead of silently running on the host.
func TestForcedSandboxWithoutProviderRefuses(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	registerExecFull(r, nil, nil, nil)

	_, err := r.Execute(context.Background(), "exec", `{"command":"echo x","sandbox":true}`)
	if err == nil || !strings.Contains(err.Error(), "sandbox required but no executor available") {
		t.Fatalf("err = %v, want sandbox-unavailable refusal", err)
	}
}

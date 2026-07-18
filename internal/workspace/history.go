package workspace

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// History is an opt-in per-scope version history for workspace directories,
// backed by git bare repositories stored OUTSIDE the workspace (default
// ~/.fastclaw/workspace-history/<scope>.git).
//
// Design notes (why this shape):
//   - A .git directory inside the workspace would be visible to the agent
//     itself: file tools would list it, the model could read or corrupt it,
//     and it would inflate listings and context. The bare repo must live
//     outside any scope the agent can see.
//   - Commits happen at turn boundary (after each agent run), not per tool
//     write. Sandbox exec writes happen inside the container over the bind
//     mount and are invisible to this process, so per-write hooks can never
//     be complete. A turn-boundary snapshot captures file tools, exec, and
//     uploads with one well-defined consistency point.
//   - Everything is best-effort: history failures must never fail or block
//     an agent run. Callers log and move on.
//
// Rollback is manual for now (git --git-dir=<repo> --work-tree=<dir> log /
// checkout); a workspace-panel entry can come later.
type History struct {
	root string
}

// NewHistory returns a History storing bare repositories under root
// (one <scope>.git per workspace scope).
func NewHistory(root string) *History {
	return &History{root: root}
}

// Commit snapshots workTree into the scope's bare repository. The first
// call initializes the repo; clean trees are skipped (no empty commits).
// Any error is returned for the caller to log — never fatal.
func (h *History) Commit(ctx context.Context, scope, workTree, message string) error {
	if scope == "" || workTree == "" {
		return fmt.Errorf("workspace history: scope and workTree required")
	}
	info, err := os.Stat(workTree)
	if err != nil || !info.IsDir() {
		return nil // nothing to snapshot
	}
	repo := filepath.Join(h.root, scope+".git")
	if _, err := os.Stat(repo); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(repo), 0o755); err != nil {
			return err
		}
		// NOTE: git rejects init combined with --work-tree
		// ("GIT_WORK_TREE not allowed without GIT_DIR"), so init uses the
		// plain path form and only later commands carry --git-dir/--work-tree.
		if out, err := runGit(ctx, nil, "init", "--bare", repo); err != nil {
			return fmt.Errorf("git init: %s: %w", out, err)
		}
	}
	gitDir := "--git-dir=" + repo
	workTreeArg := "--work-tree=" + workTree
	if out, err := runGit(ctx, nil, gitDir, workTreeArg, "add", "-A"); err != nil {
		return fmt.Errorf("git add: %s: %w", out, err)
	}
	status, err := runGit(ctx, nil, gitDir, workTreeArg, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %s: %w", status, err)
	}
	if strings.TrimSpace(status) == "" {
		return nil // clean tree — skip empty commit
	}
	// Inline identity so this works without the operator's global git config.
	out, err := runGit(ctx, nil, gitDir, workTreeArg,
		"-c", "user.name=fastclaw", "-c", "user.email=fastclaw@localhost",
		"commit", "-m", message, "--quiet")
	if err != nil {
		return fmt.Errorf("git commit: %s: %w", out, err)
	}
	slog.Info("workspace history committed", "scope", scope, "repo", repo)
	return nil
}

// Entry is one snapshot commit, newest first when returned by List.
type Entry struct {
	Hash    string `json:"hash"`
	Message string `json:"message"`
	Time    int64  `json:"time"`
}

// List returns the scope's snapshot commits, newest first. No history
// yields an empty slice (not an error).
func (h *History) List(ctx context.Context, scope string) ([]Entry, error) {
	repo := h.RepoPath(scope)
	if _, err := os.Stat(repo); os.IsNotExist(err) {
		return nil, nil
	}
	out, err := runGit(ctx, nil, "--git-dir="+repo, "log", "--pretty=%H|%s|%ct")
	if err != nil {
		return nil, fmt.Errorf("git log: %s: %w", out, err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var entries []Entry
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		var t int64
		fmt.Sscanf(parts[2], "%d", &t)
		entries = append(entries, Entry{Hash: parts[0], Message: parts[1], Time: t})
	}
	return entries, nil
}

// Restore checks the whole worktree out to the given commit. The commit
// hash is validated to prevent flag injection.
func (h *History) Restore(ctx context.Context, scope, workTree, commit string) error {
	if !isHexHash(commit) {
		return fmt.Errorf("workspace history: invalid commit hash")
	}
	repo := h.RepoPath(scope)
	if _, err := os.Stat(repo); os.IsNotExist(err) {
		return fmt.Errorf("workspace history: no history for scope %q", scope)
	}
	out, err := runGit(ctx, nil, "--git-dir="+repo, "--work-tree="+workTree, "checkout", commit, "--", ".")
	if err != nil {
		return fmt.Errorf("git checkout: %s: %w", out, err)
	}
	slog.Info("workspace history restored", "scope", scope, "commit", commit)
	return nil
}

func isHexHash(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// RepoPath returns the bare repository path for a scope (used by tests and
// by any future rollback UI).
func (h *History) RepoPath(scope string) string {
	return filepath.Join(h.root, scope+".git")
}

// runGit executes a git command with a hard timeout and returns combined
// output. env is reserved for callers that need to override the environment.
func runGit(ctx context.Context, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if env != nil {
		cmd.Env = env
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

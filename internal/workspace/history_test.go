package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitAvailable(t *testing.T) {
	t.Helper()
	if err := exec.Command("git", "--version").Run(); err != nil {
		t.Skip("git not available")
	}
}

func gitLog(t *testing.T, repo, workTree string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+repo, "--work-tree="+workTree, "log", "--oneline").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, out)
	}
	return string(out)
}

func TestHistoryCommitSnapshotsWorkTree(t *testing.T) {
	gitAvailable(t)
	root := t.TempDir()
	workTree := filepath.Join(root, "ws")
	if err := os.MkdirAll(workTree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workTree, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHistory(filepath.Join(root, "history"))

	if err := h.Commit(context.Background(), "s1", workTree, "turn 1"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	repo := h.RepoPath("s1")
	if info, err := os.Stat(repo); err != nil || !info.IsDir() {
		t.Fatalf("bare repo should exist at %s", repo)
	}
	// 工作区内不得出现 .git（agent 可见性红线）
	if _, err := os.Stat(filepath.Join(workTree, ".git")); !os.IsNotExist(err) {
		t.Fatal("workTree must not contain .git")
	}
	if log := gitLog(t, repo, workTree); !strings.Contains(log, "turn 1") {
		t.Fatalf("expected 'turn 1' commit, got: %s", log)
	}
}

func TestHistorySkipsEmptyCommit(t *testing.T) {
	gitAvailable(t)
	root := t.TempDir()
	workTree := filepath.Join(root, "ws")
	if err := os.MkdirAll(workTree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workTree, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHistory(filepath.Join(root, "history"))

	if err := h.Commit(context.Background(), "s1", workTree, "turn 1"); err != nil {
		t.Fatal(err)
	}
	if err := h.Commit(context.Background(), "s1", workTree, "turn 2"); err != nil {
		t.Fatal(err)
	}

	if log := gitLog(t, h.RepoPath("s1"), workTree); strings.Count(log, "\n") != 0 && len(strings.Split(strings.TrimSpace(log), "\n")) != 1 {
		t.Fatalf("clean tree must not produce an empty commit: %s", log)
	}
}

func TestHistorySecondCommitCapturesModificationAndRollback(t *testing.T) {
	gitAvailable(t)
	root := t.TempDir()
	workTree := filepath.Join(root, "ws")
	if err := os.MkdirAll(workTree, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workTree, "a.txt")
	if err := os.WriteFile(file, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHistory(filepath.Join(root, "history"))

	if err := h.Commit(context.Background(), "s1", workTree, "turn 1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("v2-corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.Commit(context.Background(), "s1", workTree, "turn 2"); err != nil {
		t.Fatal(err)
	}

	repo := h.RepoPath("s1")
	if log := gitLog(t, repo, workTree); len(strings.Split(strings.TrimSpace(log), "\n")) != 2 {
		t.Fatalf("expected 2 commits: %s", log)
	}
	// 回滚到 run-1 的内容
	out, err := exec.Command("git", "--git-dir="+repo, "--work-tree="+workTree, "checkout", "HEAD~1", "--", "a.txt").CombinedOutput()
	if err != nil {
		t.Fatalf("checkout: %v: %s", err, out)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v1" {
		t.Fatalf("rollback should restore v1, got %q", data)
	}
}

func TestHistoryListAndRestore(t *testing.T) {
	gitAvailable(t)
	root := t.TempDir()
	workTree := filepath.Join(root, "ws")
	if err := os.MkdirAll(workTree, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workTree, "a.txt")
	h := NewHistory(filepath.Join(root, "history"))
	ctx := context.Background()

	if err := os.WriteFile(file, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.Commit(ctx, "s1", workTree, "turn 1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.Commit(ctx, "s1", workTree, "turn 2"); err != nil {
		t.Fatal(err)
	}

	entries, err := h.List(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Message != "turn 2" || entries[0].Time == 0 {
		t.Fatalf("newest entry first with timestamp: %+v", entries[0])
	}
	// 无历史的 scope 返回空列表而非错误
	if empty, err := h.List(ctx, "nope"); err != nil || len(empty) != 0 {
		t.Fatalf("missing scope should list empty: %v %v", empty, err)
	}

	if err := h.Restore(ctx, "s1", workTree, entries[1].Hash); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v1" {
		t.Fatalf("restore should bring back v1, got %q", data)
	}
}

func TestHistoryRestoreRejectsBadHash(t *testing.T) {
	gitAvailable(t)
	h := NewHistory(t.TempDir())
	if err := h.Restore(context.Background(), "s1", t.TempDir(), "main; rm -rf /"); err == nil {
		t.Fatal("invalid hash must be rejected")
	}
}

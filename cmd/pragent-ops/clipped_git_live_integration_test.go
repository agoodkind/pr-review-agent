//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (fixture *clippedLiveFixture) commit(t *testing.T, ctx context.Context, content, subject string) string {
	t.Helper()
	worktree := os.Getenv("PR_AGENT_LIVE_CLIPPED_WORKTREE")
	if !filepath.IsAbs(worktree) {
		t.Fatal("PR_AGENT_LIVE_CLIPPED_WORKTREE must identify the fixture's absolute worktree path.")
	}
	if clippedLiveGit(t, ctx, worktree, "status", "--porcelain") != "" {
		t.Fatal("The fixture worktree must be clean before each signed commit.")
	}
	if clippedLiveGit(t, ctx, worktree, "rev-parse", "HEAD") != fixture.head || clippedLiveGit(t, ctx, worktree, "branch", "--show-current") != fixture.original.Head.Ref {
		t.Fatal("The fixture worktree must use the expected branch and commit.")
	}
	remote := clippedLiveGit(t, ctx, worktree, "remote", "get-url", "--push", "origin")
	if remote != "git@github.com:"+fixture.opts.repository+".git" && remote != "https://github.com/"+fixture.opts.repository+".git" {
		t.Fatal("The fixture worktree must push to the selected GitHub repository.")
	}
	file := filepath.Join(worktree, filepath.FromSlash(fixture.filePath))
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatal("The fixture path must identify an existing regular file.")
	}
	if err := os.WriteFile(file, []byte(content), info.Mode().Perm()); err != nil {
		t.Fatal("The fixture file could not be updated.")
	}
	clippedLiveGit(t, ctx, worktree, "add", "--", fixture.filePath)
	if clippedLiveGit(t, ctx, worktree, "diff", "--cached", "--name-only") == "" {
		return fixture.head
	}
	clippedLiveGit(t, ctx, worktree, "commit", "-S", "-m", subject+"\n\nCo-authored-by: Codex <noreply@openai.com>")
	clippedLiveGit(t, ctx, worktree, "push", "--force-with-lease=refs/heads/"+fixture.original.Head.Ref+":"+fixture.head, "origin", "HEAD:refs/heads/"+fixture.original.Head.Ref)
	return clippedLiveGit(t, ctx, worktree, "rev-parse", "HEAD")
}

func clippedLiveGit(t *testing.T, ctx context.Context, directory string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Git failed during fixture %s: %.512s", arguments[0], output)
	}
	return strings.TrimSpace(string(output))
}

func (fixture *clippedLiveFixture) awaitHead(t *testing.T, parent context.Context, previous string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		current := fixture.pullRequest(t, ctx).Head.SHA
		if current == fixture.head {
			return
		}
		if current != previous {
			t.Fatal("An unrelated commit changed the pull request head after the fixture push.")
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	t.Fatal("GitHub did not report the signed fixture commit within thirty seconds.")
}

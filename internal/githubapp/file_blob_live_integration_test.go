//go:build integration

package githubapp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
)

// TestGetLargePinnedFileFromGitHub reads actual GitHub bytes and verifies their Git object identity.
func TestGetLargePinnedFileFromGitHub(t *testing.T) {
	tokenFile := os.Getenv("PR_AGENT_LIVE_GITHUB_TOKEN_FILE")
	if tokenFile == "" {
		t.Skip("Set PR_AGENT_LIVE_GITHUB_TOKEN_FILE to run authenticated GitHub retrieval.")
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal("The GitHub credential file could not be read.")
	}
	if strings.TrimSpace(string(token)) == "" {
		t.Fatal("The GitHub credential file must contain an existing token.")
	}
	baseURL, err := url.Parse("https://api.github.com")
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(config.Config{GitHubAPIBaseURL: baseURL}, &http.Client{Timeout: time.Minute}, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	const installationID = 1
	client.tokens.set(installationID, strings.TrimSpace(string(token)), time.Now().Add(time.Hour))
	repo := domain.Repository{Owner: "torvalds", Name: "linux"}
	const path = "drivers/gpu/drm/amd/include/asic_reg/gca/gfx_8_0_sh_mask.h"
	const expectedBlob = "4070ca3a68eb9b361a45303d3ee5c44571a7f55c"
	const expectedBytes = 1136970
	const revision domain.HeadSHA = "7be4bc52fd9e0e55dc9a2e0232b89427d2c4b614"
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	content, err := client.GetFile(ctx, installationID, repo, path, revision)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if len(content) != expectedBytes {
		t.Fatalf("GitHub returned %d bytes; expected %d.", len(content), expectedBytes)
	}
	command := exec.CommandContext(ctx, "git", "hash-object", "--stdin")
	command.Stdin = bytes.NewReader(content)
	identity, err := command.Output()
	if err != nil {
		t.Fatal("Git could not calculate the retrieved file's object identity.")
	}
	if strings.TrimSpace(string(identity)) != expectedBlob {
		t.Fatal("The retrieved bytes do not match the pinned GitHub blob.")
	}
	t.Logf("Retrieved %d bytes at %s and verified blob %s.", len(content), revision, expectedBlob)
}

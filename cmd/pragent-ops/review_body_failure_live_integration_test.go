//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/githubapp"
)

func TestLiveReviewBodyFailurePreservesTypedErrorAndPrivatePayload(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_REVIEW_BODY_FAILURE_TEST") != "1" {
		t.Skip("The live review-body failure regression is not enabled.")
	}
	identifier, err := strconv.ParseInt(os.Getenv("PR_AGENT_LIVE_REVIEW_BODY_FAILURE_ID"), 10, 64)
	if err != nil || identifier <= 0 {
		t.Fatal("The regression requires an explicit positive review identifier.")
	}
	opts := liveReviewBodyOptions(t)
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal("The explicit repository is invalid.")
	}
	cfg, err := reviewBodyIdentity(opts)
	if err != nil {
		t.Fatal("The live GitHub identity could not be configured.")
	}
	client := githubapp.NewClient(cfg, &http.Client{Timeout: 30 * time.Second}, time.Now, slog.Default())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	before := liveReviewBodyCapture(t, ctx, client, opts, repo)
	selected, _ := selectReviewBodies(before.Reviews, cfg.GitHubBotLogin)
	if !slices.ContainsFunc(selected, func(item githubapp.Review) bool {
		return item.ID == identifier && item.State == "CHANGES_REQUESTED" && item.Body != ""
	}) {
		t.Fatal("The selected review must be an eligible bot rejection with a nonempty body.")
	}
	backupPath := filepath.Join(t.TempDir(), "original-review-bodies.json")
	args := []string{
		"review-bodies", "--runtime", opts.runtime, "--repository", opts.repository,
		"--pull-request", strconv.Itoa(opts.pullRequest), "--installation-id", strconv.FormatInt(opts.installationID, 10),
		"--app-private-key-file", opts.privateKeyFile, "--review-id", strconv.FormatInt(identifier, 10),
		"--apply", "--backup-file", backupPath,
	}
	failure := run(ctx, args, io.Discard, io.Discard)
	after := liveReviewBodyCapture(t, ctx, client, opts, repo)
	if failure == nil || !liveReviewBodySnapshotsEqual(before, after) {
		directory, err := os.MkdirTemp("", "pragent-review-body-unexpected-")
		if err != nil || writeReviewBodyBackup(filepath.Join(directory, "fixture-before.json"), before) != nil {
			t.Fatal("The unexpected result's original snapshot could not be retained.")
		}
		t.Fatalf("The operation unexpectedly succeeded or changed GitHub data; the original snapshot is retained in %s.", directory)
	}
	var apiError githubapp.APIError
	if !errors.As(failure, &apiError) || apiError.StatusCode != http.StatusUnprocessableEntity || apiError.Message == "" {
		t.Fatal("The public CLI error did not preserve GitHub's typed HTTP 422 failure.")
	}
	if strings.Contains(failure.Error(), apiError.Message) || strings.Contains(failure.Error(), "{") {
		t.Fatal("The public CLI error exposed the native API payload.")
	}
	data, err := os.ReadFile(backupPath + ".error.json")
	var diagnostic reviewBodyDiagnostic
	if err != nil || json.Unmarshal(data, &diagnostic) != nil || diagnostic.HTTPStatus != apiError.StatusCode || diagnostic.ReviewID != identifier || diagnostic.NativeError != apiError.Message || diagnostic.Operation != "clear_review_body" {
		t.Fatal("The private diagnostic did not retain the exact native rejection and review identity.")
	}
	info, err := os.Stat(backupPath + ".error.json")
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("The diagnostic does not restrict access to its owner.")
	}
	data, err = os.ReadFile(backupPath)
	var backup reviewBodySnapshot
	if err != nil || json.Unmarshal(data, &backup) != nil || !liveReviewBodySnapshotsEqual(before, backup) {
		t.Fatal("The backup did not retain the complete original GitHub snapshot.")
	}
	t.Logf("Review %d retained its body and CHANGES_REQUESTED state after HTTP 422; the CLI preserved the typed error and private diagnostic.", identifier)
}

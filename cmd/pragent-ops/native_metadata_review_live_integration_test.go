//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/githubapp"
)

func TestLiveEmptyNativeReviewRejectsBodyUpdates(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_EMPTY_NATIVE_BODY_TEST") != "1" {
		t.Skip("The dedicated empty review body regression is not enabled.")
	}
	opts := liveReviewBodyOptions(t)
	if opts.repository != "agoodkind/goodkind.io" || opts.pullRequest != 359 || opts.installationID != 153224986 {
		t.Fatal("The body update regression requires the closed dedicated fixture 359.")
	}
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := reviewBodyIdentity(opts)
	if err != nil {
		t.Fatal(err)
	}
	client := githubapp.NewClient(cfg, &http.Client{Timeout: 30 * time.Second}, time.Now, slog.Default())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	before := liveReviewBodyCapture(t, ctx, client, opts, repo)
	if before.PullRequest.State != "closed" {
		t.Fatal("The dedicated fixture must remain closed during this API check.")
	}
	const reviewID int64 = 5730764975
	var original githubapp.Review
	for _, item := range before.Reviews {
		if item.ID == reviewID {
			original = item
		}
	}
	if original.ID != reviewID || original.Author != cfg.GitHubBotLogin || original.State != "CHANGES_REQUESTED" || original.Body != "" {
		t.Fatal("The owned fixture review must have an empty submitted rejection body.")
	}
	directory, err := os.MkdirTemp("", "pragent-empty-native-body-")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReviewBodyBackup(filepath.Join(directory, "before.json"), before); err != nil {
		t.Fatal(err)
	}
	const body = "This closed fixture verifies that an inline rejection can receive its metadata explanation."
	_, updateErr := client.UpdateReview(ctx, opts.installationID, repo, opts.pullRequest, reviewID, body)
	after := liveReviewBodyCapture(t, ctx, client, opts, repo)
	if err := writeReviewBodyBackup(filepath.Join(directory, "after.json"), after); err != nil {
		t.Fatal(err)
	}
	var native githubapp.APIError
	if !errors.As(updateErr, &native) || native.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(native.Message, "Could not edit a review with a missing body") {
		t.Fatalf("The native empty-body update contract changed. Inspect the private evidence in %s.", directory)
	}
	data, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "error.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if !liveReviewBodySnapshotsEqual(before, after) {
		t.Fatal("The refused body update changed the closed fixture.")
	}
	t.Logf("GitHub refused the body update with HTTP422 and preserved review %d and the entire closed fixture. Private evidence: %s.", reviewID, directory)
}

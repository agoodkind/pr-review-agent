//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func TestLiveReviewBodiesPreservesEmptyReviewDecisions(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_REVIEW_BODIES_TEST") != "1" {
		t.Skip("The live review-body regression is not enabled.")
	}
	opts := liveReviewBodyOptions(t)
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal("The explicit test repository is invalid.")
	}
	cfg, err := reviewBodyIdentity(opts)
	if err != nil {
		t.Fatal("The public runtime and app key could not configure the test identity.")
	}
	client := githubapp.NewClient(cfg, &http.Client{Timeout: 30 * time.Second}, time.Now, slog.Default())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	before := liveReviewBodyCapture(t, ctx, client, opts, repo)
	if before.PullRequest.State != "open" || before.PullRequest.Draft || !before.PullRequest.EligibilityKnown || !before.PullRequest.MetadataKnown {
		t.Fatal("The selected fixture must be an open, non-draft pull request with validated metadata.")
	}
	if !liveReviewBodyFixture(t, opts, before.PullRequest.Title) {
		t.Fatal("The explicit target must be a test fixture or the configured recovery-test pull request.")
	}
	if validateReviewBodySummary(before, cfg.GitHubBotLogin) != nil {
		t.Fatal("The fixture requires one completed bot summary at the current commit.")
	}
	selected, _ := selectReviewBodies(before.Reviews, cfg.GitHubBotLogin)
	if len(selected) != 0 {
		t.Fatal("The idempotence fixture must have no bot review bodies eligible for cleanup.")
	}
	var current githubapp.Review
	var dismissed githubapp.Review
	for _, item := range before.Reviews {
		if item.Author != cfg.GitHubBotLogin || item.Body != "" {
			continue
		}
		if item.CommitID == before.PullRequest.Head && item.State == "CHANGES_REQUESTED" {
			current = item
		}
		if item.State == "DISMISSED" {
			dismissed = item
		}
	}
	if current.ID == 0 || dismissed.ID == 0 || dismissed.ID >= current.ID {
		t.Fatal("The fixture requires a current bot rejection and an earlier dismissed bot rejection with empty bodies.")
	}
	originalDismissal, found, err := client.FindReviewDismissal(ctx, opts.installationID, repo, opts.pullRequest, dismissed.ID)
	if err != nil || !found || originalDismissal.OriginalDecision != domain.ReviewDecisionRequestChanges {
		t.Fatal("The native GitHub timeline did not establish the dismissed review's original requested-changes decision.")
	}
	backupPath := filepath.Join(t.TempDir(), "original-review-bodies.json")
	args := []string{
		"review-bodies", "--runtime", opts.runtime,
		"--repository", opts.repository, "--pull-request", strconv.Itoa(opts.pullRequest),
		"--installation-id", strconv.FormatInt(opts.installationID, 10),
		"--app-private-key-file", opts.privateKeyFile, "--backup-file", backupPath,
	}
	for _, apply := range []bool{false, true} {
		commandArgs := args
		if apply {
			commandArgs = append(commandArgs, "--apply")
		}
		var output bytes.Buffer
		if run(ctx, commandArgs, &output, io.Discard) != nil {
			t.Fatalf("The real review-bodies command failed with apply=%t.", apply)
		}
		after := liveReviewBodyCapture(t, ctx, client, opts, repo)
		if !liveReviewBodySnapshotsEqual(before, after) {
			t.Fatalf("The command changed metadata, reviews, summary, or inline comments with apply=%t.", apply)
		}
		if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
			t.Fatalf("The command created a backup for already-empty bodies with apply=%t.", apply)
		}
		for _, item := range []githubapp.Review{current, dismissed} {
			if !strings.Contains(output.String(), fmt.Sprintf("Review %d has state %s and an empty body.", item.ID, item.State)) {
				t.Fatalf("The command omitted review %d or its native state with apply=%t.", item.ID, apply)
			}
		}
	}
	retainedDismissal, found, err := client.FindReviewDismissal(ctx, opts.installationID, repo, opts.pullRequest, dismissed.ID)
	if err != nil || !found || retainedDismissal != originalDismissal {
		t.Fatal("The repeated cleanup command did not retain the dismissed review's original native decision.")
	}
	t.Logf("The dry run and apply command preserved review %d state CHANGES_REQUESTED and review %d state DISMISSED with native original decision REQUEST_CHANGES.", current.ID, dismissed.ID)
}

func liveReviewBodyOptions(t *testing.T) reviewBodyOptions {
	t.Helper()
	var opts reviewBodyOptions
	opts.runtime = filepath.Join("..", "..", "runtime.json")
	opts.repository = os.Getenv("PR_AGENT_LIVE_REPOSITORY")
	opts.privateKeyFile = os.Getenv("PR_AGENT_LIVE_GITHUB_APP_KEY_FILE")
	var numberErr, installationErr error
	opts.pullRequest, numberErr = strconv.Atoi(os.Getenv("PR_AGENT_LIVE_PULL_REQUEST"))
	opts.installationID, installationErr = strconv.ParseInt(os.Getenv("PR_AGENT_LIVE_INSTALLATION_ID"), 10, 64)
	if opts.repository == "" || opts.privateKeyFile == "" || numberErr != nil || opts.pullRequest <= 0 || installationErr != nil || opts.installationID <= 0 {
		t.Fatal("The live review-body regression requires explicit repository, pull request, installation, and app key file.")
	}
	return opts
}

func liveReviewBodyFixture(t *testing.T, opts reviewBodyOptions, title string) bool {
	t.Helper()
	lowerTitle := strings.ToLower(title)
	if strings.Contains(lowerTitle, "test") || strings.Contains(lowerTitle, "fixture") {
		return true
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "cloudflare", "recovery-test.json"))
	if err != nil {
		t.Fatal("The dedicated recovery-test configuration could not be read.")
	}
	var fixture struct {
		Repository  string `json:"repository"`
		PullRequest int    `json:"pullRequest"`
	}
	if json.Unmarshal(data, &fixture) != nil || fixture.Repository == "" || fixture.PullRequest <= 0 {
		t.Fatal("The dedicated recovery-test configuration is invalid.")
	}
	return fixture.Repository == opts.repository && fixture.PullRequest == opts.pullRequest
}

func liveReviewBodyCapture(t *testing.T, ctx context.Context, client *githubapp.Client, opts reviewBodyOptions, repo domain.Repository) reviewBodySnapshot {
	t.Helper()
	snapshot, err := captureReviewBodies(ctx, client, opts.installationID, repo, opts.pullRequest)
	if err != nil {
		t.Fatalf("The live GitHub fixture snapshot could not be read: %s", liveReviewBodyReadFailure(ctx, err))
	}
	return snapshot
}

func liveReviewBodyReadFailure(ctx context.Context, failure error) string {
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("%v; polling context: %v", failure, err)
	}
	return failure.Error()
}

func liveReviewBodySnapshotsEqual(first, second reviewBodySnapshot) bool {
	first.CapturedAt = time.Time{}
	second.CapturedAt = time.Time{}
	return reflect.DeepEqual(first, second)
}

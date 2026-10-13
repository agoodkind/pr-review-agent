//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestLiveFindingDetailsRemoveAnInlineDuplicateDuringRefresh(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_FINDING_DETAILS_TEST") != "1" {
		t.Skip("The live finding details regression is not enabled.")
	}
	opts := liveReviewBodyOptions(t)
	repository, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal("The explicit test repository is invalid.")
	}
	cfg, _ := liveFailureConfig(t)
	directory, err := cloudflareops.PrivateDirectory(os.Getenv("PR_AGENT_LIVE_FINDING_DETAILS_OUTPUT_DIR"))
	if err != nil {
		t.Fatal("The private regression evidence directory could not be created.")
	}
	logPath := filepath.Join(directory, "service.ndjson")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal("The private service log could not be created.")
	}
	t.Cleanup(func() {
		if err := logFile.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error("The private service log could not be closed.")
		}
	})
	service, github := liveFailureService(cfg, slog.New(slog.NewJSONHandler(logFile, nil)))
	service.SetApproveEachCommit(false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	before := liveReviewBodyCapture(t, ctx, github, opts, repository)
	if before.PullRequest.State != "open" || before.PullRequest.Draft || !before.PullRequest.EligibilityKnown || !before.PullRequest.MetadataKnown || !liveReviewBodyFixture(t, opts, before.PullRequest.Title) {
		t.Fatal("The explicit target must be an open, non-draft test fixture with validated metadata.")
	}
	if githubapp.LatestDecisionReview(before.Reviews, cfg.GitHubBotLogin).State != "CHANGES_REQUESTED" {
		t.Fatal("The fixture requires an existing bot requested-changes decision.")
	}
	reference := domain.PullRequestRef{Repository: repository, Number: opts.pullRequest, InstallationID: opts.installationID, Head: before.PullRequest.Head}
	original := liveApprovalSummary(t, ctx, github, reference, cfg.GitHubBotLogin)
	finding := liveFindingDetailsInline(t, before.Threads, cfg.GitHubBotLogin)
	backup, err := json.Marshal(before)
	if err != nil || os.WriteFile(filepath.Join(directory, "original.json"), backup, 0o600) != nil {
		t.Fatal("The original GitHub state could not be captured privately.")
	}
	var summary review.Summary
	summary.Head = reference.Head
	summary.Decision = domain.ReviewDecisionRequestChanges
	summary.ApprovalWithheld = true
	summary.CoverageComplete = true
	summary.MinimumImportance = cfg.MinimumImportance
	summary.MetadataRevision = diff.PullRequestRevision(before.PullRequest)
	summary.Fallback = []domain.Finding{finding}
	var state marker.State
	state.LastReviewed, state.RunID, state.Status = reference.Head, "live-finding-details-seed", marker.StateDone
	seededBody := review.RenderBody(summary) + "\n\n" + marker.EncodeState(state)
	if !strings.Contains(seededBody, finding.Title) || !strings.Contains(seededBody, finding.Body) || !strings.Contains(seededBody, "<!-- pr-review-agent:fallback-record:v1 ") {
		t.Fatal("The seeded summary must display the duplicate and retain its fallback checkpoint.")
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		restored, err := github.UpdateIssueComment(cleanupCtx, reference.InstallationID, reference.Repository, original.ID, original.Body)
		if err != nil || restored != original {
			t.Error("The original editable summary could not be restored.")
		}
		after := liveReviewBodyCapture(t, cleanupCtx, github, opts, repository)
		if !liveReviewBodySnapshotsEqual(before, after) {
			t.Error("The restored GitHub metadata, reviews, comments, and threads differ from the original snapshot.")
		}
	})
	if _, err := github.UpdateIssueComment(ctx, reference.InstallationID, reference.Repository, original.ID, seededBody); err != nil {
		t.Fatal("The duplicate finding summary could not be seeded.")
	}
	_, outcome, runErr := liveApprovalRun(t, ctx, service, reference, nil)
	if runErr != nil || outcome.Disposition != domain.AssessmentCompleted {
		t.Fatal("The production service did not complete the finding refresh.")
	}
	current := liveApprovalSummary(t, ctx, github, reference, cfg.GitHubBotLogin)
	if current.ID != original.ID || strings.Contains(current.Body, finding.Title) || strings.Contains(current.Body, finding.Body) || !strings.Contains(current.Body, "<!-- pr-review-agent:fallback-record:v1 ") {
		t.Fatal("The single summary must remove the inline duplicate and retain its fallback checkpoint.")
	}
	after := liveReviewBodyCapture(t, ctx, github, opts, repository)
	if after.PullRequest != before.PullRequest || !reflect.DeepEqual(after.Reviews, before.Reviews) || !reflect.DeepEqual(after.Threads, before.Threads) {
		t.Fatal("The refresh changed pull request metadata, native reviews, or inline threads.")
	}
	if err := logFile.Sync(); err != nil {
		t.Fatal("The private service log could not be synchronized.")
	}
	liveApprovalAssertNoProviderRequests(t, logPath)
	t.Logf("The production refresh removed an inline duplicate from summary %d and preserved all review threads and decisions. Private evidence is available in %s.", original.ID, directory)
}

func liveFindingDetailsInline(t *testing.T, threads []githubapp.ReviewThread, bot string) domain.Finding {
	t.Helper()
	for _, thread := range threads {
		if thread.Resolved || thread.RootComment.Author != bot {
			continue
		}
		_, finding, err := marker.DecodeFindingBody(thread.RootComment)
		if err != nil {
			continue
		}
		return finding
	}
	t.Fatal("The fixture requires an existing open bot inline finding with a valid publication marker.")
	return domain.Finding{}
}

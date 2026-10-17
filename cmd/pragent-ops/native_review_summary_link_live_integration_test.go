//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestLiveNativeReviewSummaryLinksAndHistoricalRepair(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_NATIVE_REVIEW_LINK_TEST") != "1" {
		t.Skip("The native review summary link regression is not enabled.")
	}
	opts := liveReviewBodyOptions(t)
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := liveFailureConfig(t)
	service, client := liveFailureService(cfg, slog.Default())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	original := liveReviewBodyCapture(t, ctx, client, opts, repo)
	if !original.PullRequest.Draft || !strings.Contains(strings.ToLower(original.PullRequest.Title), "fixture") || len(original.Threads) != 0 {
		t.Fatal("The live regression requires a draft fixture without inline threads.")
	}
	directory, err := os.MkdirTemp("", "pragent-native-review-link-")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReviewBodyBackup(filepath.Join(directory, "before.json"), original); err != nil {
		t.Fatal(err)
	}
	t.Logf("The private regression evidence directory is %s.", directory)
	if os.Getenv("PR_AGENT_LIVE_NATIVE_REVIEW_LINK_CLEANUP_ONLY") == "1" {
		liveNativeRestoreOversized(t, ctx, cfg.ReviewPolicy, client, opts, repo, cfg.GitHubBotLogin)
		return
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		liveNativeRestoreOversized(t, cleanupCtx, cfg.ReviewPolicy, client, opts, repo, cfg.GitHubBotLogin)
	})
	logPath := filepath.Join(directory, "service.ndjson")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := logFile.Close(); err != nil {
			t.Error(err)
		}
	})
	logger := slog.New(slog.NewJSONHandler(logFile, nil))
	service, _ = liveFailureService(cfg, logger)
	var summary review.Summary
	summary.Head = original.PullRequest.Head
	summary.Decision = domain.ReviewDecisionRequestChanges
	summary.CoverageComplete = true
	summary.OmissionsAccepted = true
	summary.MinimumImportance = cfg.MinimumImportance
	summary.MetadataRevision = diff.PullRequestRevision(original.PullRequest)
	summary.Metadata = []domain.Finding{{
		Surface: domain.FindingPullRequestTitle, RuleID: "pr_title", StartLine: 1, EndLine: 1,
		Title: "Verify the fixture title", Body: "The fixture title exercises native review publication.",
		Importance: 10,
	}}
	var state marker.State
	state.LastReviewed, state.Status, state.RunID = summary.Head, marker.StateDone, "native-link-seed-"+rand.Text()
	var comment githubapp.IssueComment
	for _, item := range original.Comments {
		if item.Author == cfg.GitHubBotLogin && marker.HasState(item.Body) {
			if comment.ID != 0 {
				t.Fatal("The fixture contains more than one bot summary.")
			}
			comment = item
		}
	}
	seedBody := review.RenderBody(summary) + "\n" + marker.EncodeState(state)
	if comment.ID == 0 {
		comment, err = client.CreateIssueComment(ctx, opts.installationID, repo, opts.pullRequest, seedBody)
	} else {
		comment, err = client.UpdateIssueComment(ctx, opts.installationID, repo, comment.ID, seedBody)
	}
	if err != nil {
		t.Fatal(err)
	}
	var reference domain.PullRequestRef
	reference.Repository, reference.InstallationID, reference.Number, reference.Head = repo, opts.installationID, opts.pullRequest, summary.Head
	if _, err := client.SubmitReview(ctx, opts.installationID, repo, opts.pullRequest, githubapp.SubmitReviewRequest{CommitID: summary.Head, Event: domain.ReviewDecisionApprove}); err != nil {
		t.Fatal(err)
	}
	before := liveApprovalReviews(t, ctx, client, reference)
	_, outcome, err := liveApprovalRun(t, ctx, service, reference, nil)
	if err != nil || outcome.Disposition != domain.AssessmentCompleted {
		t.Fatalf("The native rejection refresh failed: %v.", err)
	}
	after := liveApprovalReviews(t, ctx, client, reference)
	if len(after) != len(before)+1 {
		t.Fatal("The refresh did not create exactly one native rejection.")
	}
	rejection := after[len(after)-1]
	expected, err := review.NativeReviewSummaryLink(cfg.ReviewPolicy, repo, opts.pullRequest, comment.ID, review.RenderVerdictBody(summary))
	if err != nil || rejection.State != "CHANGES_REQUESTED" || rejection.Body != expected {
		t.Fatal("The native rejection did not retain the configured direct summary link and recovery metadata.")
	}
	if _, _, err := liveApprovalRun(t, ctx, service, reference, nil); err != nil {
		t.Fatal(err)
	}
	if repeated := liveApprovalReviews(t, ctx, client, reference); !reflect.DeepEqual(repeated, after) {
		t.Fatal("A repeated rejection refresh changed native reviews.")
	}
	legacyBody := review.RenderVerdictBody(summary)
	legacy, err := client.UpdateReview(ctx, opts.installationID, repo, opts.pullRequest, rejection.ID, legacyBody)
	if err != nil || legacy.ID != rejection.ID || legacy.State != rejection.State {
		t.Fatal("The historical hidden-only fixture could not preserve its native identity and state.")
	}
	beforeRepair := liveReviewBodyCapture(t, ctx, client, opts, repo)
	backupPath := filepath.Join(directory, "repair-backup.json")
	args := []string{"review-bodies", "--runtime", opts.runtime, "--repository", opts.repository,
		"--pull-request", strconv.Itoa(opts.pullRequest), "--installation-id", strconv.FormatInt(opts.installationID, 10),
		"--app-private-key-file", opts.privateKeyFile, "--review-id", strconv.FormatInt(rejection.ID, 10),
		"--link-summary", "--backup-file", backupPath}
	if err := run(ctx, args, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if current := liveReviewBodyCapture(t, ctx, client, opts, repo); !liveReviewBodySnapshotsEqual(beforeRepair, current) {
		t.Fatal("The dry run changed the fixture.")
	}
	applyArgs := append(slices.Clone(args), "--apply")
	if err := run(ctx, applyArgs, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	repaired := liveReviewBodyCapture(t, ctx, client, opts, repo)
	expectedReview := legacy
	expectedReview.Body = expected
	if err := verifyReviewBodySnapshot(ctx, client, beforeRepair, []githubapp.Review{expectedReview}); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, applyArgs, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if current := liveReviewBodyCapture(t, ctx, client, opts, repo); !liveReviewBodySnapshotsEqual(repaired, current) {
		t.Fatal("A repeated repair changed the fixture.")
	}
	summary.Decision, summary.Metadata = domain.ReviewDecisionApprove, nil
	if _, err := client.UpdateIssueComment(ctx, opts.installationID, repo, comment.ID, review.RenderBody(summary)+"\n"+marker.EncodeState(state)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := liveApprovalRun(t, ctx, service, reference, nil); err != nil {
		t.Fatal(err)
	}
	approved := liveApprovalReviews(t, ctx, client, reference)
	last := approved[len(approved)-1]
	if last.State != "APPROVED" || last.Body != "" {
		t.Fatal("The completed clean refresh did not omit its native approval body.")
	}
	if _, _, err := liveApprovalRun(t, ctx, service, reference, nil); err != nil {
		t.Fatal(err)
	}
	if repeated := liveApprovalReviews(t, ctx, client, reference); !reflect.DeepEqual(repeated, approved) {
		t.Fatal("The default approval policy published another native review.")
	}
	liveNativeMissingSummaryFailure(t, ctx, cfg.ReviewPolicy, client, opts, reference, summary, comment.ID, directory, logger)
	final := liveReviewBodyCapture(t, ctx, client, opts, repo)
	liveApprovalAssertProtectedReviews(t, original.Reviews, final.Reviews)
	if !reflect.DeepEqual(final.PullRequest, original.PullRequest) || !reflect.DeepEqual(final.Threads, original.Threads) {
		t.Fatal("The regression changed source metadata or inline threads.")
	}
	if err := writeReviewBodyBackup(filepath.Join(directory, "after.json"), final); err != nil {
		t.Fatal(err)
	}
	if err := logFile.Sync(); err != nil {
		t.Fatal(err)
	}
	liveApprovalAssertNoProviderRequests(t, logPath)
	t.Logf("Summary %d and native rejection %d retained their identities. The production refresh, repair command, and repeated approval checks passed.", comment.ID, rejection.ID)
}

func liveNativeRestoreOversized(t *testing.T, ctx context.Context, policy reviewrules.Policy, client *githubapp.Client, opts reviewBodyOptions, repo domain.Repository, bot string) {
	t.Helper()
	var reference domain.PullRequestRef
	reference.Repository, reference.Number, reference.InstallationID = repo, opts.pullRequest, opts.installationID
	summary := liveApprovalSummary(t, ctx, client, reference, bot)
	reviews, err := client.ListReviews(ctx, opts.installationID, repo, opts.pullRequest)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range reviews {
		if item.Author != bot || len(item.Body) <= 10000 || !strings.Contains(item.Body, "The fixture exercises publication rejection. ") {
			continue
		}
		start := strings.Index(item.Body, "\n\n<!-- pr-review-agent:review:v1 ")
		if start < 0 {
			t.Fatal("The oversized fixture body lost its recovery checkpoint.")
		}
		body, err := review.NativeReviewSummaryLink(policy, repo, opts.pullRequest, summary.ID, item.Body[start+2:])
		if err != nil {
			t.Fatal(err)
		}
		updated, err := client.UpdateReview(ctx, opts.installationID, repo, opts.pullRequest, item.ID, body)
		expected := item
		expected.Body = body
		if err != nil || updated != expected {
			t.Fatal("The oversized fixture body could not be restored without changing its native decision.")
		}
		t.Logf("Native review %d retains state %s after restoring its small summary link.", item.ID, item.State)
	}
}

func liveNativeMissingSummaryFailure(t *testing.T, ctx context.Context, policy reviewrules.Policy, client *githubapp.Client, opts reviewBodyOptions, reference domain.PullRequestRef, summary review.Summary, summaryID int64, directory string, logger *slog.Logger) {
	t.Helper()
	summary.Decision, summary.ApprovalWithheld = domain.ReviewDecisionComment, true
	summary.Metadata = []domain.Finding{{Surface: domain.FindingPullRequestTitle, RuleID: "pr_title", StartLine: 1, EndLine: 1, Title: "Verify the fixture title", Body: "The fixture title exercises native review publication.", Importance: 10}}
	legacy, err := client.SubmitReview(ctx, opts.installationID, reference.Repository, reference.Number, githubapp.SubmitReviewRequest{CommitID: summary.Head, Event: domain.ReviewDecisionComment, Body: review.RenderVerdictBody(summary)})
	if err != nil {
		t.Fatal(err)
	}
	deletePath := "repos/" + opts.repository + "/issues/comments/" + strconv.FormatInt(summaryID, 10)
	if output, err := exec.CommandContext(ctx, "gh", "api", "--method", "DELETE", deletePath).CombinedOutput(); err != nil {
		t.Fatalf("The dedicated fixture summary could not be deleted: %v: %s.", err, output)
	}
	cfg, _ := liveFailureConfig(t)
	promptsPath := filepath.Join("..", "..", "config", "review-prompts.json")
	data, err := os.ReadFile(promptsPath)
	if err != nil {
		t.Fatal(err)
	}
	var prompts map[string]json.RawMessage
	if err := json.Unmarshal(data, &prompts); err != nil {
		t.Fatal(err)
	}
	var templates map[string]string
	if err := json.Unmarshal(prompts["templates"], &templates); err != nil {
		t.Fatal(err)
	}
	// The configured body exceeds GitHub's accepted review size.
	templates["native.summary_link"] += "\n\n" + strings.Repeat("The fixture exercises publication rejection. ", 50000)
	prompts["templates"], err = json.Marshal(templates)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(prompts)
	if err != nil {
		t.Fatal(err)
	}
	invalidPrompts := filepath.Join(directory, "oversized-review-prompts.json")
	if err := os.WriteFile(invalidPrompts, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.ReviewPolicy, err = reviewrules.LoadPolicy(filepath.Join("..", "..", "config", "review-rules.json"), invalidPrompts)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := liveFailureService(cfg, logger)
	_, outcome, failure := liveApprovalRun(t, ctx, service, reference, nil)
	var apiError githubapp.APIError
	if failure == nil || !errors.As(failure, &apiError) || (apiError.StatusCode != 422 && apiError.StatusCode != 413) || outcome.Disposition != domain.AssessmentFailed {
		t.Fatal("The missing-summary recovery did not retain the real GitHub publication refusal.")
	}
	reviews := liveApprovalReviews(t, ctx, client, reference)
	if last := reviews[len(reviews)-1]; last.ID != legacy.ID || last.State != "COMMENTED" {
		t.Fatal("The refused native publication changed the previous decision.")
	}
	recovered := liveApprovalSummary(t, ctx, client, reference, cfg.GitHubBotLogin)
	state, valid := marker.DecodeState(recovered.Body)
	if recovered.ID == summaryID || !valid || state.Status != marker.StateFailed || state.LastReviewed != reference.Head || !review.MetadataMatches(recovered.Body, reference.Head, summary.MetadataRevision) {
		t.Fatal("The refused native publication did not retain a failed summary and its recovered assessment.")
	}
	cfg.ReviewPolicy = policy
	service, _ = liveFailureService(cfg, logger)
	if _, outcome, err := liveApprovalRun(t, ctx, service, reference, nil); err != nil || outcome.Disposition != domain.AssessmentCompleted {
		t.Fatalf("The recovered assessment could not publish after restoring the configured link: %v.", err)
	}
	current := liveApprovalSummary(t, ctx, client, reference, cfg.GitHubBotLogin)
	state, valid = marker.DecodeState(current.Body)
	if current.ID != recovered.ID || !valid || state.Status != marker.StateDone {
		t.Fatal("The successful retry did not reuse and complete the recovered summary.")
	}
	t.Logf("Native review %d retained COMMENTED after GitHub HTTP%d. Recovered summary %d retained assessment metadata and completed after retry.", legacy.ID, apiError.StatusCode, recovered.ID)
}

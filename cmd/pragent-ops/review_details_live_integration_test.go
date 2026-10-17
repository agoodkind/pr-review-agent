//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

type liveDetailsFixture struct {
	opts     reviewBodyOptions
	cfg      config.Config
	client   *githubapp.Client
	service  *review.Service
	before   reviewBodySnapshot
	original githubapp.IssueComment
	expected string
	logFile  *os.File
	logPath  string
}

func TestLiveReviewDetailsSurviveRefreshFailureAndRecovery(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_REVIEW_DETAILS_TEST") != "1" {
		t.Skip("The live review-details regression is not enabled.")
	}
	fixture := liveDetailsSetup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	original := fixture.original.Body
	report, _, hasVerdict := strings.Cut(original, "<!-- pr-review-agent:verdict-section:start -->")
	if !hasVerdict {
		t.Fatal("The fixture requires a completed report and verdict section.")
	}
	report = strings.TrimSpace(report)
	details := liveDetailsSection(t, original, "<details>\n<summary>Review details</summary>", "</details>")
	metadata := liveDetailsSection(t, original, "<!-- pr-review-agent:metadata:v1 ", " -->")
	fixture.write(t, ctx, strings.Replace(original, metadata, "", 1))
	failed, failure := fixture.run(t, ctx)
	if failure == nil || failed.Disposition != domain.AssessmentFailed {
		t.Fatal("The real provider refusal did not fail the refresh.")
	}
	if err := fixture.logFile.Sync(); err != nil {
		t.Fatal("The private provider log could not be synchronized.")
	}
	liveFailureAssertProvider(t, fixture.logPath, fixture.cfg.Providers[0].ID, fixture.cfg.Providers[0].Model)
	body := fixture.capture(t, ctx)
	if !strings.Contains(body, details) || !strings.Contains(body, report) {
		t.Fatal("The failed refresh erased measured review details.")
	}
	state, valid := marker.DecodeState(body)
	if !valid || state.Status != marker.StateFailed || state.LastReviewed != fixture.before.PullRequest.Head {
		t.Fatal("The failed refresh did not retain its completed-head checkpoint.")
	}
	if !strings.Contains(body, "Review refresh failed") {
		t.Fatal("The failed refresh did not explain the operational failure.")
	}
	fixture.write(t, ctx, strings.Replace(body, marker.EncodeState(state), metadata+"\n"+marker.EncodeState(state), 1))
	completed, recoveryErr := fixture.run(t, ctx)
	if recoveryErr != nil || completed.Disposition != domain.AssessmentCompleted {
		t.Fatal("The refresh could not recover from the completed assessment without a model request.")
	}
	body = fixture.capture(t, ctx)
	liveDetailsAssertRecovered(t, body, details, report, fixture.before.PullRequest.Head)

	verdict := liveDetailsSection(t, body, "<!-- pr-review-agent:verdict-section:start -->", "<!-- pr-review-agent:verdict-section:end -->")
	state, _ = marker.DecodeState(body)
	legacyState := state
	legacyState.Status = marker.StateFailed
	legacy := strings.Replace(body, verdict, "", 1)
	legacy = strings.Replace(legacy, marker.EncodeState(state), marker.EncodeState(legacyState), 1)
	fixture.write(t, ctx, legacy)
	completed, recoveryErr = fixture.run(t, ctx)
	if recoveryErr != nil || completed.Disposition != domain.AssessmentCompleted {
		t.Fatal("The legacy failure body could not recover its completed assessment.")
	}
	liveDetailsAssertRecovered(t, fixture.capture(t, ctx), details, report, fixture.before.PullRequest.Head)
	t.Log("The real provider failure, successful refresh, and legacy recovery retained the original measured details and native reviews.")
}

func liveDetailsSetup(t *testing.T) *liveDetailsFixture {
	t.Helper()
	opts := liveReviewBodyOptions(t)
	cfg, _ := liveFailureConfig(t)
	directory, err := os.MkdirTemp("", "pragent-details-live-")
	if err != nil {
		t.Fatal("The private evidence directory could not be created.")
	}
	logPath := filepath.Join(directory, "service.ndjson")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal("The private service log could not be created.")
	}
	service, client := liveFailureService(cfg, slog.New(slog.NewJSONHandler(logFile, nil)))
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal("The fixture repository is invalid.")
	}
	before := liveReviewBodyCapture(t, t.Context(), client, opts, repo)
	if before.PullRequest.State != "open" || before.PullRequest.Draft || !liveReviewBodyFixture(t, opts, before.PullRequest.Title) || validateReviewBodySummary(before, cfg.GitHubBotLogin) != nil {
		t.Fatal("The regression requires a dedicated open fixture with a completed current-head review.")
	}
	if githubapp.LatestDecisionReview(before.Reviews, cfg.GitHubBotLogin).State != "APPROVED" {
		t.Fatal("The fixture requires an existing bot approval.")
	}
	var original githubapp.IssueComment
	for _, comment := range before.Comments {
		if comment.Author == cfg.GitHubBotLogin && marker.HasState(comment.Body) {
			original = comment
		}
	}
	if original.ID == 0 || strings.Contains(original.Body, "| Model | unknown |") || !strings.Contains(original.Body, "| Total tokens |") {
		t.Fatal("The fixture must contain measured model and token details.")
	}
	if err := writeReviewBodyBackup(filepath.Join(directory, "before.json"), before); err != nil {
		t.Fatal("The original GitHub fixture could not be backed up.")
	}
	fixture := &liveDetailsFixture{opts: opts, cfg: cfg, client: client, service: service, before: before, original: original, expected: original.Body, logFile: logFile, logPath: logPath}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		fixture.write(t, ctx, original.Body)
		if err := logFile.Close(); err != nil {
			t.Error("The private log could not be closed.")
		}
	})
	t.Logf("The private evidence directory is %s.", directory)
	return fixture
}

func (fixture *liveDetailsFixture) run(t *testing.T, ctx context.Context) (domain.AssessmentOutcome, error) {
	t.Helper()
	job := domain.ReviewJob{PullRequestRef: domain.PullRequestRef{Repository: fixture.before.Repository, Number: fixture.opts.pullRequest, InstallationID: fixture.opts.installationID, Head: fixture.before.PullRequest.Head}, DeliveryID: "live-review-details-" + rand.Text(), RefreshVerdict: true}
	admitted, accepted, err := fixture.service.Admit(ctx, job)
	if err != nil || !accepted {
		t.Fatal("The real GitHub App did not admit the refresh regression.")
	}
	outcome, runErr := fixture.service.RunWithOutcome(ctx, admitted)
	fixture.capture(t, ctx)
	return outcome, runErr
}

func (fixture *liveDetailsFixture) capture(t *testing.T, ctx context.Context) string {
	t.Helper()
	current := liveReviewBodyCapture(t, ctx, fixture.client, fixture.opts, fixture.before.Repository)
	if current.PullRequest != fixture.before.PullRequest || !reflect.DeepEqual(current.Reviews, fixture.before.Reviews) || !reflect.DeepEqual(current.Threads, fixture.before.Threads) {
		t.Fatal("The refresh changed PR metadata, native reviews, or inline threads.")
	}
	for _, comment := range current.Comments {
		if comment.ID == fixture.original.ID && comment.Author == fixture.original.Author {
			fixture.expected = comment.Body
			return comment.Body
		}
	}
	t.Fatal("The original bot summary is missing.")
	return ""
}

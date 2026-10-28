//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestLiveMixedFailureReportsTheFinalFailedOperation(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_MIXED_FAILURE_TEST") != "1" {
		t.Skip("The mixed failure regression requires an explicit fixture.")
	}
	opts := liveReviewBodyOptions(t)
	cfg, _ := liveFailureConfig(t)
	service, github := liveFailureService(cfg, slog.Default())
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	before := liveReviewBodyCapture(t, ctx, github, opts, repo)
	if before.PullRequest.State != "closed" || !before.PullRequest.Draft || !liveReviewBodyFixture(t, opts, before.PullRequest.Title) {
		t.Fatal("Select a closed draft fixture for the mixed failure regression.")
	}
	reference := domain.PullRequestRef{Repository: repo, InstallationID: opts.installationID, Number: opts.pullRequest, Head: before.PullRequest.Head}
	original := liveApprovalSummary(t, ctx, github, reference, cfg.GitHubBotLogin)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if _, err := github.UpdateIssueComment(cleanupCtx, opts.installationID, repo, original.ID, original.Body); err != nil {
			t.Error("Restore the fixture summary:", err)
		}
	})
	_, providerErr := openai.NewClient(cfg, http.DefaultClient).Review(ctx, "Review this identity function: func identity(value int) int { return value }")
	var providerFailure *openai.ProviderError
	if !errors.As(providerErr, &providerFailure) || providerFailure.StatusCode != http.StatusUnauthorized {
		t.Fatal("The real provider did not return the intended authentication refusal.")
	}
	_, publicationErr := github.SubmitReview(ctx, opts.installationID, repo, opts.pullRequest, githubapp.SubmitReviewRequest{CommitID: reference.Head, Event: domain.ReviewDecisionRequestChanges})
	var publicationFailure githubapp.APIError
	if !errors.As(publicationErr, &publicationFailure) || publicationFailure.StatusCode != http.StatusUnprocessableEntity {
		t.Fatal("GitHub did not return the intended empty-review refusal.")
	}
	job := domain.ReviewJob{PullRequestRef: reference, DeliveryID: "live-mixed-failure-" + rand.Text(), Forced: true}
	job, admitted, err := service.Admit(ctx, job)
	if err != nil || !admitted {
		t.Fatal("The mixed failure check could not be admitted.")
	}
	if err := service.Reject(ctx, job, errors.Join(providerErr, publicationErr)); err == nil {
		t.Fatal("The failed operations did not retain their error result.")
	}
	check, err := github.GetCheckRunDetails(ctx, opts.installationID, repo, job.CheckRunID)
	if err != nil || check.Run.Status != "completed" || check.Run.Conclusion != "cancelled" {
		t.Fatal("The mixed failure check did not retain its cancelled outcome.")
	}
	var data reviewrules.PromptData
	data.FailureStatus = "HTTP 422"
	data.FailureClass, err = cfg.ReviewPolicy.Render("failure.class.other", data)
	if err != nil {
		t.Fatal(err)
	}
	explanation, err := cfg.ReviewPolicy.Render("failure.service", data)
	if err != nil || !strings.HasPrefix(check.Summary, explanation) {
		t.Fatal("The final GitHub publication error incorrectly blamed the earlier provider failure.")
	}
	after := liveReviewBodyCapture(t, ctx, github, opts, repo)
	if !reflect.DeepEqual(before.PullRequest, after.PullRequest) || !reflect.DeepEqual(before.Threads, after.Threads) || !reflect.DeepEqual(before.Reviews, after.Reviews) || len(before.Comments) != len(after.Comments) {
		t.Fatal("The mixed failure changed fixture metadata, reviews, threads, or comment count.")
	}
	t.Logf("Check %d attributed the final failure to the GitHub operation after the real provider HTTP 401.", job.CheckRunID)
}

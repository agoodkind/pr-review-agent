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

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

func TestLivePartialReviewSurvivesPromptFailureAndInvalidSavedState(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_PARTIAL_PROGRESS_TEST") != "1" {
		t.Skip("The live partial-progress regression is not enabled.")
	}
	opts := liveReviewBodyOptions(t)
	cfg, _ := liveFailureConfig(t)
	cfg.ReviewMaxPromptBytes = 1
	directory, err := os.MkdirTemp("", "pragent-partial-progress-live-")
	if err != nil {
		t.Fatal("The private evidence directory could not be created.")
	}
	logFile, err := os.OpenFile(filepath.Join(directory, "service.ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal("The private service log could not be created.")
	}
	service, client := liveFailureService(cfg, slog.New(slog.NewJSONHandler(logFile, nil)))
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal("The explicit fixture repository is invalid.")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	before := liveReviewBodyCapture(t, ctx, client, opts, repo)
	if !liveReviewBodyFixture(t, opts, before.PullRequest.Title) || githubapp.LatestDecisionReview(before.Reviews, cfg.GitHubBotLogin).State != "APPROVED" {
		t.Fatal("The regression requires a dedicated fixture with an existing bot approval.")
	}
	var original githubapp.IssueComment
	for _, comment := range before.Comments {
		if comment.Author == cfg.GitHubBotLogin && marker.HasState(comment.Body) {
			original = comment
		}
	}
	state, valid := marker.DecodeState(original.Body)
	if original.ID == 0 || !valid || !strings.Contains(original.Body, "<!-- pr-review-agent:metadata:v1 ") {
		t.Fatal("The fixture requires an existing summary with valid saved metadata.")
	}
	if err := writeReviewBodyBackup(filepath.Join(directory, "before.json"), before); err != nil {
		t.Fatal("The original fixture snapshot could not be saved.")
	}
	t.Cleanup(func() {
		recovery, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		if _, err := client.UpdateIssueComment(recovery, opts.installationID, repo, original.ID, original.Body); err != nil {
			t.Error("The original fixture summary could not be restored.")
		}
		after := liveReviewBodyCapture(t, recovery, client, opts, repo)
		if !liveReviewBodySnapshotsEqual(before, after) {
			t.Error("The regression did not restore the original fixture records.")
		}
		if err := logFile.Close(); err != nil {
			t.Error("The private service log could not be closed.")
		}
	})
	state.LastReviewed = ""
	state.RunID = "partial-progress-setup"
	state.Status = marker.StateReviewing
	state.Pending = []string{"pending-source-fragment"}
	state.Completed = []string{"completed-metadata-fragment"}
	prefix, _, found := strings.Cut(original.Body, "\n<!-- pr-review-agent:state:v1 ")
	if !found {
		t.Fatal("The fixture state marker is not separate from its saved report.")
	}
	seeded := strings.TrimSpace(prefix) + "\n" + marker.EncodeState(state)
	if _, err := client.UpdateIssueComment(ctx, opts.installationID, repo, original.ID, seeded); err != nil {
		t.Fatal("The partial checkpoint could not be stored in GitHub.")
	}
	for range 2 {
		job := domain.ReviewJob{PullRequestRef: domain.PullRequestRef{Repository: repo, Number: opts.pullRequest, InstallationID: opts.installationID, Head: before.PullRequest.Head}, DeliveryID: "partial-progress-failure-" + rand.Text(), Forced: true}
		admitted, accepted, err := service.Admit(ctx, job)
		if err != nil || !accepted {
			t.Fatal("The real GitHub service did not admit the prompt-budget regression.")
		}
		outcome, runErr := service.RunWithOutcome(ctx, admitted)
		if runErr == nil || outcome.Disposition != domain.AssessmentFailed {
			t.Fatal("The prompt budget did not fail the actual fixture before analysis.")
		}
		body := livePartialSummary(t, ctx, client, opts, original.ID)
		stored, valid := marker.DecodeState(body)
		if !valid || stored.Status != marker.StateFailed || stored.RunID != job.DeliveryID || stored.LastReviewed != "" || !reflect.DeepEqual(stored.Completed, state.Completed) || !reflect.DeepEqual(stored.Pending, state.Pending) {
			t.Fatal("The failed run discarded the partial checkpoint.")
		}
		if !strings.HasPrefix(body, strings.TrimSpace(prefix)) || strings.Count(body, "<!-- pr-review-agent:maintenance-failure:start -->") != 1 {
			t.Fatal("The failed run erased saved results or repeated its status notice.")
		}
	}
	invalid := strings.Replace(seeded, "run=partial-progress-setup", "run=", 1)
	if _, valid := marker.DecodeState(invalid); valid {
		t.Fatal("The damaged checkpoint must fail state decoding.")
	}
	if _, err := client.UpdateIssueComment(ctx, opts.installationID, repo, original.ID, invalid); err != nil {
		t.Fatal("The damaged checkpoint could not be stored in GitHub.")
	}
	job := domain.ReviewJob{PullRequestRef: domain.PullRequestRef{Repository: repo, Number: opts.pullRequest, InstallationID: opts.installationID, Head: before.PullRequest.Head}, DeliveryID: "partial-progress-invalid-" + rand.Text(), Forced: true}
	admitted, accepted, err := service.Admit(ctx, job)
	if err != nil || !accepted {
		t.Fatal("The real GitHub service did not admit the invalid-state regression.")
	}
	outcome, runErr := service.RunWithOutcome(ctx, admitted)
	if runErr == nil || outcome.Disposition != domain.AssessmentFailed || !strings.Contains(runErr.Error(), "saved review state") {
		t.Fatal("The invalid checkpoint did not stop the review before analysis.")
	}
	if livePartialSummary(t, ctx, client, opts, original.ID) != invalid {
		t.Fatal("The failed read replaced the original saved review data.")
	}
	t.Logf("The real GitHub regression retained saved results across repeated prompt failures and an invalid checkpoint. Private evidence: %s", directory)
}

func livePartialSummary(t *testing.T, ctx context.Context, client *githubapp.Client, opts reviewBodyOptions, identifier int64) string {
	t.Helper()
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal("The explicit fixture repository is invalid.")
	}
	comments, err := client.ListIssueComments(ctx, opts.installationID, repo, opts.pullRequest)
	if err != nil {
		t.Fatal("The real GitHub fixture summary could not be read.")
	}
	for _, comment := range comments {
		if comment.ID == identifier {
			return comment.Body
		}
	}
	t.Fatal("The original fixture summary is missing.")
	return ""
}

package review

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

var errReviewContextChanged = errors.New("the saved review context requires reassessment")

func (service *Service) reassessChangedContext(ctx context.Context, job domain.ReviewJob, reviews []githubapp.Review, settings reviewSettings) error {
	current, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		slog.WarnContext(ctx, "Load pull request for changed review context failed", "err", err)
		return fmt.Errorf("load changed review context: %w", err)
	}
	if current.Head != job.Head {
		return service.cancelCheck(ctx, job, job.CheckRunID)
	}
	if err := service.upsertSummaryCommentFrom(ctx, job, func(state marker.State, body string) summaryCommentContent {
		state.LastReviewed = ""
		state.Pending = nil
		state.Completed = nil
		state.Unread = nil
		state.PolicyRevision = ""
		state.Status = marker.StateReviewing
		return summaryCommentContent{Prose: stripDurableStateMarker(body), State: state}
	}); err != nil {
		return err
	}
	var check githubapp.CheckRun
	check.ID = job.CheckRunID
	check.Head = job.Head
	check.Name = service.checkName
	check.Status = job.CheckRunStatus
	check.Conclusion = job.CheckRunConclusion
	startedAt := service.now()
	progress := service.newProgress(job, settings, startedAt)
	return service.reviewOwedWork(ctx, job, check, current, reviews, startedAt, progress, settings)
}

func bindCheckpointPolicy(state marker.State, pullRequest githubapp.PullRequest, policy reviewrules.Policy, settings reviewSettings) marker.State {
	revision := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", policy.Fingerprint(), pullRequest.Base, pullRequest.Head, diff.PullRequestRevision(pullRequest), settings.minimumImportance)
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(revision)))
	return state.WithPolicyRevision(digest)
}

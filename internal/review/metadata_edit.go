package review

import (
	"context"
	"log/slog"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

func (service *Service) reviewMetadataEdit(ctx context.Context, job domain.ReviewJob,
	checkRun githubapp.CheckRun, pullRequest githubapp.PullRequest, startedAt time.Time,
	progress *reviewProgress, settings reviewSettings,
) error {
	state, found, err := service.readState(ctx, job)
	if err != nil {
		gklog.L(ctx).WarnContext(ctx, "read metadata edit coverage", slog.String("err", err.Error()))
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureSummary, err)
	}
	if found && state.Status == marker.StateDone && state.LastReviewed == job.Head && len(state.Pending) == 0 {
		return service.refreshReviewedCheck(ctx, job, nil, settings)
	}
	// Missing file coverage requires normal review, including unfinished chunks.
	reviews, _, err := service.loadReviewHistory(ctx, job, job.Head, progress)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureReviews, err)
	}
	return service.reviewOwedWork(ctx, job, checkRun, pullRequest, reviews, startedAt, progress, settings)
}

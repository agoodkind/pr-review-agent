package review

import (
	"context"
	"errors"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

var errCurrentPullRequestIncomplete = errors.New("current pull request response is incomplete")

func (service *Service) reviewSnapshot(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
	head domain.HeadSHA,
	startedAt time.Time,
	progress *reviewProgress,
	settings reviewSettings,
) error {
	// A forced run must never stop on a conclusion an earlier run reached, since
	// redoing that run is the whole point. Admission gives it its own check run,
	// so this is normally not a completed check at all; the guard keeps that
	// true if a forced job ever reaches here carrying one.
	if !job.Forced && !job.AutomaticReassessment && service.checkAlreadySucceeded(ctx, checkRun) {
		// A prior successful check does not override a refreshed withheld verdict.
		return service.refreshReviewedCheck(ctx, job, nil, settings)
	}

	pullRequest, err := service.github.GetPullRequest(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
	)
	if err != nil {
		return service.failCheck(
			ctx,
			job,
			checkRun.ID,
			progress.summary(service.now()),
			checkFailurePullRequest,
			err,
		)
	}

	if pullRequest.Head != head {
		return service.cancelCheck(ctx, job, checkRun.ID)
	}

	if job.AutomaticReassessment && (!pullRequest.EligibilityKnown || !pullRequest.MetadataKnown) {
		return service.failCheck(
			ctx,
			job,
			checkRun.ID,
			progress.summary(service.now()),
			checkFailureRefresh,
			errCurrentPullRequestIncomplete,
		)
	}

	if job.AutomaticReassessment &&
		(pullRequest.State != "open" || pullRequest.Merged || pullRequest.Draft) {
		return service.cancelCheck(ctx, job, checkRun.ID)
	}

	progress.reached("the pull request")

	if job.MetadataOnly {
		return service.reviewMetadataEdit(
			ctx,
			job,
			checkRun,
			pullRequest,
			startedAt,
			progress,
			settings,
		)
	}

	if job.AutomaticReassessment && job.RefreshVerdict {
		return service.refreshReviewedCheck(ctx, job, nil, settings)
	}

	reviews, reviewed, err := service.loadReviewHistory(ctx, job, head, progress)
	if err != nil {
		return service.failCheck(
			ctx,
			job,
			checkRun.ID,
			progress.summary(service.now()),
			checkFailureReviews,
			err,
		)
	}

	if reviewed {
		return service.refreshReviewedCheck(ctx, job, reviews, settings)
	}

	return service.reviewOwedWork(
		ctx,
		job,
		checkRun,
		pullRequest,
		reviews,
		startedAt,
		progress,
		settings,
	)
}

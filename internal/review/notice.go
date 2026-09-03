package review

// This file handles what a failed review leaves behind on the pull request.
//
// A review that fails has no verdict, and it has also not earned the right to
// withdraw one. It reports the cause in two places a reader already looks: the
// red check run, and the one top level comment. It touches no review object at
// all, because a run that could not finish knows nothing new about the head,
// and a verdict it invented or withdrew there would be a judgment nobody made.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

// failCheck ends one run with its cause reported and no review object touched.
//
// The check run and the comment now say the same amount, because they are
// equally public. Both name what stopped, in wording this service wrote, and
// point at the run identifier. Neither reprints the sentence the provider
// supplied: that sentence is text nobody here has read, and a check run
// outlives the run exactly as a comment does.
func (service *Service) failCheck(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	progress Summary,
	stage string,
	cause error,
) error {
	logger := gklog.L(ctx)
	progress.Failed = true
	title := failureTitle(stage, cause)
	checkSummary := publicFailureDetail(job) + "\n\n" + RenderDetails(progress)
	var completeErr error
	if checkRunID != 0 {
		completeErr = service.completeCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			checkRunID,
			"failure",
			title,
			checkSummary,
		)
		if completeErr != nil {
			logger.ErrorContext(ctx, "complete failed check run", slog.String("err", completeErr.Error()))
		}
	}
	service.writeFailureSummary(ctx, job, progress, title, publicFailureDetail(job))
	if completeErr != nil {
		return fmt.Errorf("complete check run: %w", completeErr)
	}
	logger.ErrorContext(ctx, "review job failed", slog.String("err", cause.Error()))
	return cause
}

// failureTitle names why a review stopped, in the one line a reader sees in the
// checks list before opening anything.
//
// A stage name alone tells the reader where the run was rather than what went
// wrong, so the classes this service can recognize are named first and the
// stage is the fallback. The provider's own sentence is not one of the options:
// it can carry the request it failed on, an internal endpoint, or a credential,
// and a check run is as public and as permanent as a comment.
//
// A failed model call never reaches here. It leaves its chunk pending rather
// than failing the run, so the neutral check reports it instead.
func failureTitle(stage string, cause error) string {
	switch {
	case usageExceeded(cause):
		return checkFailureUsage
	case errors.Is(cause, context.DeadlineExceeded):
		return checkFailureDeadline
	case isChunkPanic(cause):
		return checkFailurePanic
	}
	if stage == "" {
		return checkSummaryFailure
	}
	return stage
}

// chunkFailureReason names why chunks went unread, in wording this service
// wrote, or an empty string when nothing classifies.
//
// It classifies rather than quotes for the same reason failureTitle does.
// Exhausted usage is the largest single cause in production, and a reader who
// sees only a chunk count cannot tell it apart from a provider outage.
func chunkFailureReason(failures []chunkFailure) string {
	for _, failure := range failures {
		switch {
		case usageExceeded(failure.err):
			return checkFailureUsage
		case errors.Is(failure.err, context.DeadlineExceeded):
			return checkFailureDeadline
		}
	}
	return ""
}

// publicFailureDetail points a reader at the cause instead of reprinting it.
//
// A model provider error can carry the request it failed on, an internal
// endpoint, or a credential, and none of that can be unpublished once it is in
// a pull request comment. The run identifier is enough to pull the whole cause
// out of the service log, which stays private.
func publicFailureDetail(job domain.ReviewJob) string {
	if job.DeliveryID == "" {
		return "The cause is recorded in this service's log for this run."
	}
	return "The cause is recorded in this service's log, under run identifier `" + job.DeliveryID + "`."
}

// usageExceededError is any provider error that reports exhausted usage.
type usageExceededError interface {
	UsageExceeded() bool
}

func usageExceeded(cause error) bool {
	var target usageExceededError
	if !errors.As(cause, &target) {
		return false
	}
	return target.UsageExceeded()
}

// writeFailureSummary states why the review stopped, in the service's one top
// level comment.
//
// It carries the checkpoint forward exactly as the comment already holds it,
// both the last reviewed commit and the pending chunk list. Whatever chunks
// this run finished are already recorded there, and whatever it did not remain
// owed, so the next run neither repeats work already done nor skips work never
// done. A failure here is logged and never masks the cause the caller reports.
func (service *Service) writeFailureSummary(
	ctx context.Context,
	job domain.ReviewJob,
	progress Summary,
	title string,
	detail string,
) {
	ctx, cancel := detachFromReviewDeadline(ctx, service.checkCompletionTimeout)
	defer cancel()

	logger := gklog.L(ctx)
	prose := RenderFailureBody(progress, title, detail)
	err := service.upsertSummaryCommentFrom(ctx, job, func(existing marker.State) summaryCommentContent {
		return summaryCommentContent{
			Prose: prose,
			State: marker.State{
				LastReviewed: existing.LastReviewed,
				RunID:        job.DeliveryID,
				Status:       marker.StateFailed,
				Pending:      existing.Pending,
				// The chunks already read carry forward for the same reason the
				// pending ones do. Dropping them here would send the next run
				// over work this pull request has already paid for.
				Completed: existing.Completed,
				// So does the forcing delivery. A failed run is not that delivery
				// finishing, and losing the record would make its own resume clear
				// the very checkpoint this notice is preserving.
				ForcedBy: existing.ForcedBy,
			},
		}
	})
	if err != nil {
		logger.ErrorContext(ctx, "write failure summary", slog.String("err", err.Error()))
		return
	}
	logger.InfoContext(ctx, "failure summary written", slog.Bool("visible", true))
}

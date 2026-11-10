package review

import (
	"context"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func (service *Service) publishStandingVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
	summary Summary,
	reviews []githubapp.Review,
	logger *slog.Logger,
) error {
	var err error

	standing := service.standingVerdict(job, reviews, summary)

	withdrawnByHuman := false
	if summary.Decision == domain.ReviewDecisionRequestChanges {
		withdrawnByHuman, err = service.preserveDismissedRejection(ctx, job, reviews)
		if err != nil {
			return service.failCheck(ctx, job, checkRun.ID, summary, checkFailurePublish, err)
		}
	}

	decisionState := reviewStateFor(summary.Decision)

	unchanged := standing.found && !standing.withdrawn &&
		standing.review.State == decisionState &&
		latestBotVerdictState(reviews, service.botLogin) == decisionState
	switch {
	case summary.Decision == domain.ReviewDecisionComment:
		if err := service.validateInlineVerdict(ctx, job, summary); err != nil {
			return service.failCheck(ctx, job, checkRun.ID, summary, checkFailurePublish, err)
		}
	case withdrawnByHuman:
		logger.InfoContext(ctx, "review verdict withheld", "reason", "block_withdrawn_by_hand")
	case unchanged:
		if err := service.updateStandingVerdict(ctx, job, standing.review, summary); err != nil {
			return service.failCheck(ctx, job, checkRun.ID, summary, checkFailurePublish, err)
		}

		logger.InfoContext(
			ctx,
			"review verdict unchanged",
			slog.Int64("review_id", standing.review.ID),
			slog.String("event", string(summary.Decision)),
		)
	default:
		publishedReview, err := service.submitInlineBackedVerdict(ctx, job, summary)
		if err != nil {
			return service.failCheck(ctx, job, checkRun.ID, summary, checkFailurePublish, err)
		}

		logger.InfoContext(
			ctx,
			"review published",
			slog.Int64("review_id", publishedReview.ID),
			slog.String("event", string(summary.Decision)),
			slog.Int("streamed_comments", len(summary.Published)),
			slog.Any("blocking", summary.Blocking),
		)
	}

	return nil
}

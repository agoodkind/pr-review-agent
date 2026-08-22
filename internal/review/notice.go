package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

const (
	// approvedReviewState is the GitHub state of a review that approved a head.
	approvedReviewState = "APPROVED"
	// dismissalMessage explains a withdrawn approval to everyone who can see it.
	dismissalMessage = "This approval covered an earlier head. " +
		"The review of the current head did not finish, so the approval no longer stands."
)

// listReviewsForFailure reads the existing reviews once, on a context that
// outlives the cancelled review, so both the approval withdrawal and the
// visible notice work from one read.
func (service *Service) listReviewsForFailure(
	ctx context.Context,
	job domain.ReviewJob,
) ([]githubapp.Review, error) {
	logger := gklog.L(ctx)
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), service.checkCompletionTimeout)
	defer cancel()

	reviews, err := service.github.ListReviews(readCtx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(readCtx, "load reviews for failure", slog.String("err", err.Error()))
		return nil, fmt.Errorf("load reviews for failure: %w", err)
	}
	return reviews, nil
}

// publishFailureNotice rewrites the single visible summary so the pull request
// states why the review stopped. A notice failure never masks the review cause.
func (service *Service) publishFailureNotice(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
	progress Summary,
	title string,
	detail string,
) {
	logger := gklog.L(ctx)
	noticeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), service.checkCompletionTimeout)
	defer cancel()

	body := RenderFailureBody(progress, title, detail)
	updated, err := service.updateSummaryReview(noticeCtx, job, reviews, body)
	if err != nil {
		logger.ErrorContext(noticeCtx, "update failure notice", slog.String("err", err.Error()))
		return
	}
	if updated {
		logger.InfoContext(noticeCtx, "failure notice updated", slog.Bool("visible", true))
		return
	}

	if _, err := service.github.SubmitReview(
		noticeCtx,
		job.InstallationID,
		job.Repository,
		job.Number,
		githubapp.SubmitReviewRequest{
			CommitID: job.Head,
			Body:     body,
			Event:    domain.ReviewDecisionComment,
			Comments: nil,
		},
	); err != nil {
		logger.ErrorContext(noticeCtx, "publish failure notice", slog.String("err", err.Error()))
		return
	}
	logger.InfoContext(noticeCtx, "failure notice published", slog.Bool("visible", true))
}

// dismissStaleApprovals withdraws every approval the service can no longer
// stand behind, and reports whether any of them survived.
//
// Editing a review body cannot change its state, so an approval of an earlier
// head keeps satisfying branch protection after the current head failed to
// review. The service can hold more than one approval at a time, because a
// later decision review carries its own approval separately from the review
// that owns the visible summary body. Every one of them counts, so every one
// is dismissed.
func (service *Service) dismissStaleApprovals(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
) error {
	logger := gklog.L(ctx)
	survivors := make([]error, 0)
	dismissed := make([]int64, 0)
	for _, item := range reviews {
		if item.Author != service.botLogin || item.State != approvedReviewState {
			continue
		}
		if dismissErr := service.github.DismissReview(
			ctx,
			job.InstallationID,
			job.Repository,
			job.Number,
			item.ID,
			dismissalMessage,
		); dismissErr != nil {
			logger.ErrorContext(
				ctx,
				"dismiss stale approval",
				slog.Int64("review_id", item.ID),
				slog.String("err", dismissErr.Error()),
			)
			survivors = append(survivors, fmt.Errorf("review %d: %w", item.ID, dismissErr))
			continue
		}
		dismissed = append(dismissed, item.ID)
	}
	if len(dismissed) > 0 {
		logger.InfoContext(ctx, "stale approvals dismissed", slog.Any("review_ids", dismissed))
	}
	return errors.Join(survivors...)
}

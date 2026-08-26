package review

// This file handles what a failed review leaves behind on the pull request.
//
// A review that fails has no verdict. The check run says so, and a visible
// notice says why. Neither of those touches the verdict an earlier review
// already left, and that verdict keeps counting: an approval keeps satisfying
// branch protection, and a changes-requested keeps blocking the merge. Both
// are withdrawn here.

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
	// changesRequestedReviewState is the GitHub state of a review that blocks a
	// pull request until it is satisfied or dismissed.
	changesRequestedReviewState = "CHANGES_REQUESTED"
	// approvalDismissalMessage explains a withdrawn approval on the timeline.
	approvalDismissalMessage = "This approval covered an earlier head. " +
		"The review of the current head did not finish, so the approval no longer stands."
	// blockDismissalMessage explains a withdrawn block on the timeline. Without
	// it the pull request stays blocked by a verdict nobody can act on, because
	// the findings that justified it were never re-checked on this head.
	blockDismissalMessage = "This review requested changes on an earlier head. " +
		"The review of the current head did not finish, so the request no longer stands. " +
		"The next successful review publishes its own verdict."
)

// standingVerdict reports whether one review state still counts toward the pull
// request's review decision. GitHub keeps the latest approval or block per
// reviewer; a comment review never replaces either, so a comment cannot clear
// one and is not a standing verdict itself.
func standingVerdict(state string) bool {
	return state == approvedReviewState || state == changesRequestedReviewState
}

// dismissalMessageFor states why one verdict was withdrawn.
func dismissalMessageFor(state string) string {
	if state == approvedReviewState {
		return approvalDismissalMessage
	}
	return blockDismissalMessage
}

// listReviewsForFailure reads the existing reviews once, on a context that
// outlives the cancelled review, so both the verdict withdrawal and the visible
// notice work from one read.
func (service *Service) listReviewsForFailure(
	ctx context.Context,
	job domain.ReviewJob,
) ([]githubapp.Review, error) {
	logger := gklog.L(ctx)
	reviews, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "load reviews for failure", slog.String("err", err.Error()))
		return nil, fmt.Errorf("load reviews for failure: %w", err)
	}
	return reviews, nil
}

// dismissStaleVerdicts withdraws every verdict the service can no longer stand
// behind, and reports every dismissal that failed.
//
// The service can hold more than one verdict at a time, because a later
// decision review carries its own state separately from the review that owns
// the visible summary body. Every one of them counts toward the pull request's
// review decision, so every one is dismissed.
func (service *Service) dismissStaleVerdicts(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
) error {
	logger := gklog.L(ctx)
	survivors := make([]error, 0)
	dismissed := make([]string, 0)
	for _, item := range reviews {
		if item.Author != service.botLogin || !standingVerdict(item.State) {
			continue
		}
		// A verdict on this same head came from a review that finished. This run
		// failing says nothing about that one, so withdrawing it would discard a
		// judgment the service still stands behind.
		if item.CommitID == job.Head {
			continue
		}
		if dismissErr := service.github.DismissReview(
			ctx,
			job.InstallationID,
			job.Repository,
			job.Number,
			item.ID,
			dismissalMessageFor(item.State),
		); dismissErr != nil {
			logger.ErrorContext(
				ctx,
				"dismiss stale verdict",
				slog.Int64("review_id", item.ID),
				slog.String("state", item.State),
				slog.String("err", dismissErr.Error()),
			)
			survivors = append(survivors, fmt.Errorf("review %d: %w", item.ID, dismissErr))
			continue
		}
		dismissed = append(dismissed, fmt.Sprintf("%d:%s", item.ID, item.State))
	}
	// The state travels with each id, so a reader can tell a withdrawn approval
	// from a withdrawn block without opening the pull request.
	logger.InfoContext(
		ctx,
		"stale verdicts dismissed",
		slog.Int("count", len(dismissed)),
		slog.Any("reviews", dismissed),
	)
	return errors.Join(survivors...)
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

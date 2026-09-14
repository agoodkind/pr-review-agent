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
	unsupportedInlineVerdictMessage = "The current review has no actionable inline finding to support requested changes."
	withheldApprovalMessage         = "The current review cannot approve this head. The review summary explains what remains unresolved."
)

// validateInlineVerdict checks live thread state before changing a verdict.
func (service *Service) validateInlineVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	summary Summary,
) error {
	if summary.Decision != domain.ReviewDecisionRequestChanges && summary.Decision != domain.ReviewDecisionComment {
		return nil
	}
	logger := gklog.L(ctx)
	threads, err := service.github.ListReviewThreads(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "read inline findings before verdict", slog.String("err", err.Error()))
		return fmt.Errorf("read inline findings before verdict: %w", err)
	}
	if hasUnresolvedBotThread(threads, service.botLogin) {
		return nil
	}
	if summary.Decision == domain.ReviewDecisionRequestChanges {
		if err := service.withdrawUnsupportedVerdicts(ctx, job, false); err != nil {
			return err
		}
		err := errors.New("cannot request changes without an actionable inline finding")
		logger.ErrorContext(ctx, "refuse unsupported verdict", slog.String("err", err.Error()))
		return err
	}
	return service.withdrawUnsupportedVerdicts(ctx, job, summary.ApprovalWithheld)
}

// A COMMENT does not replace a standing GitHub approval or requested-changes review.
func (service *Service) withdrawUnsupportedVerdicts(
	ctx context.Context,
	job domain.ReviewJob,
	approvalWithheld bool,
) error {
	logger := gklog.L(ctx)
	reviews, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "read reviews before withholding verdict", slog.String("err", err.Error()))
		return fmt.Errorf("read reviews before withholding verdict: %w", err)
	}
	withdrawn := 0
	for _, item := range reviews {
		if item.Author != service.botLogin {
			continue
		}
		message := unsupportedInlineVerdictMessage
		if item.State == reviewStateApproved && approvalWithheld {
			message = withheldApprovalMessage
		} else if item.State != reviewStateChangesRequested {
			continue
		}
		if _, err := service.github.DismissReview(
			ctx, job.InstallationID, job.Repository, job.Number, item.ID, message,
		); err != nil {
			logger.ErrorContext(ctx, "withdraw unsupported verdict", slog.Int64("review_id", item.ID), slog.String("err", err.Error()))
			return fmt.Errorf("withdraw unsupported verdict %d: %w", item.ID, err)
		}
		withdrawn++
	}
	if withdrawn > 0 {
		logger.InfoContext(ctx, "review verdicts withdrawn", slog.Int("count", withdrawn))
	}
	return nil
}

func (service *Service) submitInlineBackedVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	summary Summary,
) (githubapp.Review, error) {
	logger := gklog.L(ctx)
	if err := service.validateInlineVerdict(ctx, job, summary); err != nil {
		return emptyReview(), err
	}
	review, err := service.github.SubmitReview(ctx, job.InstallationID, job.Repository, job.Number, githubapp.SubmitReviewRequest{
		CommitID: summary.Head,
		Body:     RenderVerdictBody(summary),
		Event:    summary.Decision,
		Comments: nil,
	})
	if err != nil {
		logger.ErrorContext(ctx, "submit inline-backed verdict", slog.String("err", err.Error()))
		return emptyReview(), fmt.Errorf("submit inline-backed verdict: %w", err)
	}
	return review, nil
}

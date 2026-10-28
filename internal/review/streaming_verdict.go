package review

import (
	"context"
	"fmt"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func (service *Service) postInlineFinding(ctx context.Context, job domain.ReviewJob, head domain.HeadSHA, comment githubapp.InlineComment, pass *chunkPass) error {
	logger := gklog.L(ctx)
	pass.nativeVerdictMu.Lock()
	submitted, err := service.establishInlineRejection(ctx, job, head, comment, pass)
	pass.nativeVerdictMu.Unlock()
	if err != nil || submitted {
		return err
	}
	if err := service.github.CreateReviewComment(ctx, job.InstallationID, job.Repository, job.Number, head, comment); err != nil {
		logger.WarnContext(ctx, "publish inline finding failed", "path", comment.Path, "line", comment.Line, "err", err)
		return fmt.Errorf("publish inline finding: %w", err)
	}
	return nil
}

// establishInlineRejection publishes one finding immediately and omits the review body.
func (service *Service) establishInlineRejection(ctx context.Context, job domain.ReviewJob, head domain.HeadSHA, comment githubapp.InlineComment, pass *chunkPass) (bool, error) {
	logger := gklog.L(ctx)
	if pass.nativeRejection {
		return false, nil
	}
	reviews, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.WarnContext(ctx, "read standing review before publishing finding", "err", err)
		return false, fmt.Errorf("read standing review before publishing finding: %w", err)
	}
	latest := githubapp.LatestDecisionReview(reviews, service.botLogin)
	if latest.State == reviewStateChangesRequested {
		pass.nativeRejection = true
		return false, nil
	}
	preserve, err := service.preserveDismissedRejection(ctx, job, reviews)
	if err != nil || preserve {
		return false, err
	}
	if err := service.confirmHead(ctx, job, head); err != nil {
		return false, err
	}
	published, err := service.github.SubmitReview(ctx, job.InstallationID, job.Repository, job.Number, githubapp.SubmitReviewRequest{
		CommitID: head,
		Body:     "",
		Event:    domain.ReviewDecisionRequestChanges,
		Comments: []githubapp.InlineComment{comment},
	})
	if err != nil {
		logger.WarnContext(ctx, "publish finding with requested changes failed", "path", comment.Path, "line", comment.Line, "err", err)
		return true, fmt.Errorf("publish finding with requested changes: %w", err)
	}
	pass.nativeRejection = true
	logger.InfoContext(ctx, "inline rejection published", "review_id", published.ID, "path", comment.Path, "line", comment.Line)
	return true, nil
}

package review

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
)

func (service *Service) confirmHead(ctx context.Context, job domain.ReviewJob, head domain.HeadSHA) error {
	logger := gklog.L(ctx)
	pullRequest, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if ctxErr := ctx.Err(); ctxErr != nil {
		logger.ErrorContext(ctx, "confirm head before posting findings", slog.String("err", ctxErr.Error()))
		return fmt.Errorf("confirm head: %w", ctxErr)
	}
	if err != nil {
		logger.ErrorContext(ctx, "read head before posting findings", slog.String("err", err.Error()))
		return nil
	}
	if pullRequest.Head != head {
		return errHeadMoved
	}
	return nil
}

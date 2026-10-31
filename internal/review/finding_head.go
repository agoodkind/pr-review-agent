package review

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func (service *Service) publicationPullRequest(ctx context.Context, job domain.ReviewJob, revision string) (githubapp.PullRequest, error) {
	pullRequest, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		slog.WarnContext(ctx, "Read pull request before publication failed", slog.String("err", err.Error()))
		return pullRequest, fmt.Errorf("refresh publication pull request: %w", err)
	}
	if pullRequest.Head != job.Head || diff.PullRequestRevision(pullRequest) != revision {
		return pullRequest, errHeadMoved
	}
	return pullRequest, nil
}

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

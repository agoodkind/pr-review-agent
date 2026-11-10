package review

import (
	"context"
	"fmt"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
)

func withRepositorySnapshot(
	ctx context.Context,
	github GitHub,
	ref domain.PullRequestRef,
) (context.Context, func(), error) {
	logger := gklog.L(ctx)

	snapshotContext, closeSnapshot, err := github.OpenRepositorySnapshot(
		ctx,
		ref.InstallationID,
		ref.Repository,
		ref.Head,
	)
	if err != nil {
		logger.WarnContext(ctx, "Open review repository snapshot failed", "err", err)
		return ctx, nil, fmt.Errorf("open review repository snapshot: %w", err)
	}

	return snapshotContext, func() {
		if err := closeSnapshot(); err != nil {
			logger.ErrorContext(ctx, "Delete temporary repository snapshot failed", "err", err)
		}
	}, nil
}

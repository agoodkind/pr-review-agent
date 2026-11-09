package reconcile

import (
	"context"
	"errors"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

type resolutionProgress struct {
	errors          []error
	resolutions     []domain.ThreadResolution
	resolvedThreads []resolvedThreadTrace
}

func (service *Service) resolveRemoved(
	ctx context.Context,
	job domain.ReviewJob,
	currentHead domain.HeadSHA,
	threads []githubapp.ReviewThread,
	removed []domain.OwnedThread,
	logger *slog.Logger,
	progress *resolutionProgress,
) bool {
	for _, thread := range removed {
		resolution := domain.ThreadResolution{
			ThreadNodeID: thread.NodeID,
			Resolution:   domain.ResolutionResolved,
			Reason:       "finding anchor no longer exists",
		}
		progress.resolutions = append(progress.resolutions, resolution)
		if err := service.resolveAtHead(
			ctx,
			job,
			currentHead,
			thread,
			threads,
			logger,
		); err != nil {
			progress.errors = append(progress.errors, err)
			if errors.Is(err, errHeadChanged) {
				return true
			}

			continue
		}

		progress.resolvedThreads = append(
			progress.resolvedThreads,
			resolvedThreadTrace{
				NodeID:    thread.NodeID,
				CommentID: thread.RootComment.DatabaseID,
			},
		)
	}

	return false
}

package reconcile

import (
	"context"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

type reconciliationPlan struct {
	prepared []preparedThread
	removed  []domain.OwnedThread
	batches  []preparedBatch
}

func (service *Service) prepareReconciliation(
	ctx context.Context,
	job domain.ReviewJob,
	pullRequest githubapp.PullRequest,
	currentHead domain.HeadSHA,
	owned []domain.OwnedThread,
	changedFiles []githubapp.ChangedFile,
	maximumBytes int,
	prepared []preparedThread,
) (reconciliationPlan, error) {
	removed := make([]domain.OwnedThread, 0)

	for _, thread := range owned {
		contextText, state := service.loadThreadContext(
			ctx,
			job,
			thread,
			currentHead,
			changedFiles,
			maximumBytes,
		)
		switch state {
		case threadContextRemoved:
			removed = append(removed, thread)

			continue
		case threadContextUnavailable:
			continue
		case threadContextPresent:
		}

		prepared = append(
			prepared,
			preparedThread{
				thread: thread,
				text:   formatThreadSection(thread, currentHead, contextText, service.botLogin),
			},
		)
	}

	batches, budgetErr := boundPreparedThreads(
		service.model,
		prepared,
		pullRequest,
		service.policy,
		maximumBytes,
	)

	return reconciliationPlan{
		prepared: prepared,
		removed:  removed,
		batches:  batches,
	}, budgetErr
}

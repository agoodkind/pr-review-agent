package review

import (
	"context"

	"goodkind.io/pr-review-agent/internal/domain"
)

// CancelMetadataCheck uses a skipped conclusion so admission does not resume obsolete edits.
func (service *Service) CancelMetadataCheck(ctx context.Context, job domain.ReviewJob) error {
	return service.completeCheckRun(ctx, job.InstallationID, job.Repository, job.CheckRunID,
		"skipped", "Review superseded", "The service admitted another check for the newest title and description.")
}

package diff

import (
	"context"
	"fmt"

	"goodkind.io/gklog"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// CollectInspectionFile reads supporting source and attributes without adding diff anchors.
func (collector *Collector) CollectInspectionFile(
	ctx context.Context,
	ref domain.PullRequestRef,
	filePath string,
) (FileContext, error) {
	logger := gklog.L(ctx)
	var file FileContext
	if _, err := domain.ParseHeadSHA(string(ref.Head)); err != nil {
		logger.WarnContext(ctx, "Inspection head validation failed", "err", err)
		return file, fmt.Errorf("inspection requires an immutable head: %w", err)
	}

	var pullRequest githubapp.PullRequest
	pullRequest.Head = ref.Head
	recovery := newPatchRecovery(collector.source, ref, pullRequest, "")
	content, err := recovery.readFile(ctx, filePath, ref.Head)
	if err != nil {
		logger.WarnContext(
			ctx,
			"Read pinned inspection source failed",
			"path",
			filePath,
			"err",
			err,
		)

		return file, fmt.Errorf("read pinned inspection source %s: %w", filePath, err)
	}

	file.Path = filePath
	file.Status = "context"
	file.CurrentContent = string(content.Data)
	file.sourceMetadata = content.Metadata
	file.CoverageComplete = true
	var changed githubapp.ChangedFile
	changed.Path = filePath
	file.Attributes, err = newRepositoryAttributes(collector.source, ref).forChange(
		ctx,
		changed,
		recovery,
	)
	if err != nil {
		logger.WarnContext(
			ctx,
			"Read pinned inspection attributes failed",
			"path",
			filePath,
			"err",
			err,
		)

		return file, fmt.Errorf("read pinned inspection attributes %s: %w", filePath, err)
	}

	return file, nil
}

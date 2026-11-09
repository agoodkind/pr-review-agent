package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func appendOmissionPreviews(
	metadata *strings.Builder,
	hunks []unreadHunk,
	listed int,
	files []diff.FileContext,
	metadataBudget int,
	settings reviewrules.FileClassificationPolicy,
	policy reviewrules.Policy,
	assessments [][]domain.FileAssessment,
) {
	fileIndex := BuildFileIndex(files)
	seen := make(map[string]bool)

	for _, hunk := range hunks[:listed] {
		file, found := fileIndex[hunk.Path]
		if !found || file.ClassificationContent() == "" || seen[hunk.Path] ||
			excludedPromptContent(file, policy, assessments) {
			continue
		}

		seen[hunk.Path] = true
		content := file.ClassificationContent()
		header := fmt.Sprintf(
			"Available file content excerpt for %s (%d bytes in the file; "+
				"this excerpt is not a diff and cannot anchor findings):\n",
			escapeOmissionPromptText(hunk.Path, policy),
			len(content),
		)
		previewBudget := metadataBudget - metadata.Len() - len(header) - len("\n\n")
		if previewBudget <= 0 {
			break
		}

		preview := omissionContentPreview(
			content,
			min(settings.MaximumPreviewBytes, previewBudget),
			policy,
		)
		metadata.WriteString(header)
		metadata.WriteString(preview)
		metadata.WriteString("\n\n")
	}

	for _, file := range files {
		if seen[file.Path] || file.ClassificationContent() == "" ||
			excludedPromptContent(file, policy, assessments) {
			continue
		}

		header := fmt.Sprintf(
			"Related evidence ID: related:%s\nChanged parent file: %s\nAvailable file content excerpt:\n",
			assessmentIdentity(file, policy),
			escapeOmissionPromptText(file.Path, policy),
		)
		remaining := metadataBudget - metadata.Len() - len(header) - len("\n\n")
		if remaining <= 0 {
			break
		}

		preview := omissionContentPreview(
			file.ClassificationContent(),
			min(settings.MaximumPreviewBytes, remaining),
			policy,
		)
		metadata.WriteString(header)
		metadata.WriteString(preview)
		metadata.WriteString("\n\n")
	}
}

func (service *Service) pendingScopeNotice(
	ctx context.Context,
	job domain.ReviewJob,
	state marker.State,
	shortfall structuralShortfall,
	failures []chunkFailure,
	cause error,
) (string, string, error) {
	var scopeData reviewrules.PromptData
	scopeData.Input = renderUnreadHunks(shortfall.Hunks)
	scopeData.Index = len(shortfall.Hunks)
	scopeData.Total = len(state.Pending)
	notice, err := renderPrompt(service.reviewPolicy, "coverage.pending", scopeData)
	if err != nil {
		return "", "", err
	}

	title, err := renderPrompt(service.reviewPolicy, "coverage.check_title", scopeData)
	if err != nil {
		return "", "", err
	}

	if reason := chunkFailureReason(failures); reason != "" {
		notice = reason + "\n\n" + notice
	}

	if cause != nil {
		notice += "\n\n" + service.terminalFailureDetail(ctx, job, cause)
	}

	return notice, title, nil
}

func (service *Service) completeIncompletePublication(
	ctx context.Context,
	publication pendingScopePublication,
	progress *reviewProgress,
	shortfall structuralShortfall,
	dismissErr error,
	logger *slog.Logger,
) error {
	if err := service.publishPendingScope(ctx, publication, progress); err != nil {
		return errors.Join(err, dismissErr)
	}

	logger.InfoContext(
		ctx,
		"review head holds changes this service cannot read",
		slog.Int("unread_hunks", len(shortfall.Hunks)),
		slog.Any("unread_paths", shortfall.paths()),
		slog.Int("pending", len(publication.state.Pending)),
		slog.Int64("check_run_id", publication.checkRunID),
	)
	if dismissErr != nil {
		logger.WarnContext(
			ctx,
			"unread-content review withdrawal failed",
			slog.String("err", dismissErr.Error()),
		)

		return &reportedFailureError{cause: dismissErr}
	}

	return nil
}

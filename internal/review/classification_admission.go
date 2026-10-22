package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
)

func (service *Service) classifyLargeInput(ctx context.Context, input diff.ReviewInput, settings reviewSettings) (diff.ReviewInput, []domain.FileAssessment, []string, error) {
	logger := gklog.L(ctx)
	contentInput := input
	contentInput.Files = make([]diff.FileContext, 0, len(input.Files))
	var assessments []domain.FileAssessment
	var models []string
	for _, file := range input.Files {
		if len(file.Patch) <= settings.maximumPromptBytes {
			contentInput.Files = append(contentInput.Files, file)
			continue
		}
		callCtx, cancel := modelRequestContext(ctx, service.model, settings.chunkTimeout)
		completion, err := AssessFileContent(callCtx, service.model, input.PullRequest, input.Files, file.Path, service.reviewPolicy, settings.maximumPromptBytes)
		cancel()
		if errors.Is(err, ErrAssessmentIncomplete) {
			logger.WarnContext(ctx, "Large file requires content review because its assessment did not establish an exclusion", slog.String("path", file.Path))
			contentInput.Files = append(contentInput.Files, file)
			continue
		}
		if err != nil {
			logger.WarnContext(ctx, "Large file assessment failed", "path", file.Path, "err", err)
			return input, nil, nil, fmt.Errorf("assess large file %s: %w", file.Path, err)
		}
		if completion.Model != "" && !slices.Contains(models, completion.Model) {
			models = append(models, completion.Model)
		}
		assessments = append(assessments, completion.Result.FileAssessments...)
		if !completion.Result.OmissionsAcceptable {
			contentInput.Files = append(contentInput.Files, file)
		}
	}
	return contentInput, assessments, models, nil
}

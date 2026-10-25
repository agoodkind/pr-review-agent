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
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func (service *Service) classifyInput(ctx context.Context, input diff.ReviewInput, settings reviewSettings) (diff.ReviewInput, []domain.FileAssessment, []string, int, error) {
	logger := gklog.L(ctx)
	assessments, unresolvedFiles, err := PlanFileAssessments(input, service.reviewPolicy)
	if err != nil {
		return input, nil, nil, 0, err
	}
	var models []string
	requests := 0
	for _, file := range unresolvedFiles {
		callCtx, cancel := modelRequestContext(ctx, service.model, settings.chunkTimeout)
		completion, err := AssessFileContent(callCtx, service.model, input.PullRequest, input.Files, file.Path, service.reviewPolicy, settings.maximumPromptBytes, assessments)
		cancel()
		if completion.Model != "" {
			requests++
		}
		if errors.Is(err, ErrAssessmentIncomplete) {
			attributes := []slog.Attr{slog.String("path", file.Path), slog.Any("err", err)}
			var rejection *AssessmentValidationError
			if errors.As(err, &rejection) {
				attributes = append(attributes, slog.String("assessment_code", string(rejection.Code)), slog.String("assessment_field", rejection.Field), slog.Int("assessment_observed", rejection.Observed), slog.Int("assessment_limit", rejection.Limit))
			}
			logger.LogAttrs(ctx, slog.LevelWarn, "File classification remains unresolved; ordinary source review will continue", attributes...)
			unresolved, fallbackErr := UnresolvedFileAssessment(file, service.reviewPolicy)
			if fallbackErr != nil {
				return input, nil, nil, requests, fallbackErr
			}
			completion.Result.FileAssessments = []domain.FileAssessment{unresolved}
		} else if err != nil {
			logger.WarnContext(ctx, "File classification failed", "path", file.Path, "err", err)
			return input, nil, nil, requests, fmt.Errorf("assess file %s: %w", file.Path, err)
		}
		if completion.Model != "" && !slices.Contains(models, completion.Model) {
			models = append(models, completion.Model)
		}
		assessments = append(assessments, completion.Result.FileAssessments...)
	}
	contentInput, assessments, err := PlanFileContentReview(input, assessments, service.reviewPolicy)
	return contentInput, assessments, models, requests, err
}

// UnresolvedFileAssessment records an incomplete decision against current file evidence.
func UnresolvedFileAssessment(file diff.FileContext, policy reviewrules.Policy) (domain.FileAssessment, error) {
	var data reviewrules.PromptData
	reason, err := renderPrompt(policy, "coverage.classification_incomplete", data)
	if err != nil {
		return domain.FileAssessment{}, err
	}
	return domain.FileAssessment{
		ContentReviewRequirement: domain.ContentReviewUncertain,
		Reviewed:                 false, Path: file.Path, Identity: assessmentIdentity(file, policy),
		ContentKind: reviewrules.ContentUnknown, Action: domain.FileActionGap,
		Reason: reason, Evidence: []string{assessmentEvidenceID(file, policy)},
	}, nil
}

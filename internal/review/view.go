package review

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	outputtemplates "goodkind.io/pr-review-agent/config/templates"
)

var errReviewOutputRender = errors.New("render review output")

func renderOutputTemplate(name string, data reflect.Value) (string, error) {
	text, err := outputtemplates.Render(name, data)
	if err != nil {
		slog.Warn(
			"render review output",
			slog.String("template", name),
			slog.String("err", err.Error()),
		)

		return "", fmt.Errorf("%w: %w", errReviewOutputRender, err)
	}

	return text, nil
}

func renderOutputText(text string) (string, error) {
	switch text {
	case checkSummaryFailure, checkSummaryCancelled, checkSummarySkipped,
		checkFailureReviews, checkFailurePullRequest, checkFailureReconcile,
		checkFailureDiff, checkFailureSkip, checkFailureAnalysis, checkFailureRefresh,
		checkFailureThreads, checkFailureSummary, checkFailurePublish,
		checkFailureUsage, checkFailureDailyBudget, checkFailureUnavailable,
		checkFailureDeadline, checkTitleAlreadyReviewed, checkFailureMixedUsage,
		checkFailureRateLimited, checkFailureMixedRateLimit, checkFailurePanic,
		oversizedHunkReason, binaryFileReason, patchAbsentReason,
		patchUnreadableReason, contentMissingReason, truncatedAnswerReason,
		unreviewedHeadReason, unsupportedInlineVerdictMessage, withheldApprovalMessage,
		"output.assessment-validation-title", "output.already_reviewed":
		return renderOutputTemplate(text, reflect.ValueOf(nil))
	}

	return text, nil
}

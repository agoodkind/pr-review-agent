package review

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

type inspectionContextKey struct{}

type inspectionState struct {
	inspector Inspector
	requests  int
	bytes     int
	completed map[InspectionRequest]struct{}
	evidence  strings.Builder
	support   string
}

func withInspection(
	ctx context.Context,
	github GitHub,
	ref domain.PullRequestRef,
	files []diff.FileContext,
	policy reviewrules.Policy,
	maximumBytes int,
) (context.Context, error) {
	if policy.Limits().InspectionRequests == 0 {
		return ctx, nil
	}

	source, available := github.(InspectionSource)
	if !available {
		return ctx, errors.New("this review cannot retrieve required supporting evidence")
	}

	inspector, err := NewInspector(
		source,
		ref,
		files,
		policy,
		min(policy.Limits().InspectionBytes, maximumBytes),
		policy.Limits().InspectionRequests,
	)
	if err != nil {
		logger := gklog.L(ctx)
		logger.WarnContext(ctx, "Create required evidence inspector failed", "err", err)

		return ctx, fmt.Errorf("create required evidence inspector: %w", err)
	}

	state := new(inspectionState)
	state.inspector = inspector
	state.completed = make(map[InspectionRequest]struct{})

	return context.WithValue(ctx, inspectionContextKey{}, state), nil
}

func inspectReviewEvidence(
	ctx context.Context,
	requests []InspectionRequest,
	required string,
	policy reviewrules.Policy,
) (string, error) {
	state, available := ctx.Value(inspectionContextKey{}).(*inspectionState)
	if !available {
		return "", errors.New("required evidence inspection is unavailable for this review")
	}

	if len(requests) > policy.Limits().InspectionRequests-state.requests {
		return "", errors.New("required evidence exceeds the configured inspection request limit")
	}

	for _, request := range requests {
		if err := request.Validate(); err != nil {
			return "", err
		}

		if len(request.Reason) > policy.FileAssessmentMaximumReasonBytes() {
			return "", errors.New("inspection reason exceeds the configured assessment limit")
		}
	}

	for _, request := range requests {
		state.requests++
		key := request

		key.Reason = ""
		if _, cached := state.completed[key]; cached {
			continue
		}

		result, err := state.inspector.Inspect(ctx, request)
		if err != nil {
			logger := gklog.L(ctx)
			logger.WarnContext(
				ctx,
				"Required evidence inspection failed",
				"operation",
				request.Operation,
				"err",
				err,
			)

			return "", fmt.Errorf("required evidence inspection failed: %w", err)
		}

		if len(result) > policy.Limits().InspectionBytes-state.bytes {
			return "", errors.New("required evidence exceeds the configured inspection byte limit")
		}

		state.completed[key] = struct{}{}
		state.bytes += len(result)
		state.evidence.WriteString(result)
		state.evidence.WriteByte('\n')
		gklog.L(ctx).InfoContext(
			ctx,
			"Review supporting evidence inspected",
			"operation",
			request.Operation,
			"inspection_requests",
			state.requests,
			"evidence_bytes",
			state.bytes,
		)
	}

	var data reviewrules.PromptData

	data.Total = policy.Limits().InspectionRequests - state.requests
	data.ContentBytes = policy.Limits().InspectionBytes - state.bytes
	data.Input = policy.WrapUntrusted(policy.EscapeUntrusted(state.evidence.String()))

	support, err := renderPrompt(policy, "inspection.input", data)
	if err != nil {
		return "", err
	}

	required = strings.TrimSuffix(required, state.support)
	state.support = support

	return required + support, nil
}

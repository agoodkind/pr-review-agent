package review

import (
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// RequestSizer measures the complete provider request, including instructions and schemas.
type RequestSizer interface {
	ReviewRequestBytes(string) (int, error)
}

// BoundReviewPrompt preserves required evidence and fits optional context into the full request.
func BoundReviewPrompt(model Model, required, optional string, maximumBytes int) (string, error) {
	measure := func(prompt string) (int, error) {
		if sizer, ok := model.(RequestSizer); ok {
			return sizer.ReviewRequestBytes(prompt)
		}
		return len(prompt), nil
	}
	size, err := measure(required)
	if err != nil {
		return "", err
	}
	if size > maximumBytes {
		return "", &diff.PromptSizeError{Actual: size, Limit: maximumBytes}
	}
	best := required
	lower, upper := 0, len(optional)
	for lower <= upper {
		middle := lower + (upper-lower)/2
		candidate := required + truncateUTF8(optional, middle)
		size, err := measure(candidate)
		if err != nil {
			return "", err
		}
		if size <= maximumBytes {
			best = candidate
			lower = middle + 1
		} else {
			upper = middle - 1
		}
	}
	return best, nil
}

// BoundModelReviewChunks includes provider request overhead before partitioning content.
func BoundModelReviewChunks(model Model, chunks []diff.Chunk, files []diff.FileContext, minimumImportance int, disputes string, policy reviewrules.Policy, maximumBytes int) ([]diff.Chunk, error) {
	return BoundModelReviewChunksWithContext(model, chunks, files, minimumImportance, "", disputes, policy, maximumBytes)
}

// BoundModelReviewChunksWithContext reserves required evidence before optional context.
func BoundModelReviewChunksWithContext(model Model, chunks []diff.Chunk, files []diff.FileContext, minimumImportance int, required, optional string, policy reviewrules.Policy, maximumBytes int) ([]diff.Chunk, error) {
	notice := ""
	if optional != "" {
		var err error
		var data reviewrules.PromptData
		notice, err = renderPrompt(policy, "review.context_omitted", data)
		if err != nil {
			return nil, err
		}
	}
	bounded, err := diff.BoundChunks(chunks, files, maximumBytes, func(chunk diff.Chunk) (int, error) {
		support := required
		if optional != "" {
			support += policy.WrapUntrusted("") + notice
		}
		prompt, err := renderChunkPrompt(chunk, minimumImportance, support, policy)
		if err != nil {
			return 0, err
		}
		return modelReviewRequestBytes(model, prompt)
	})
	if err != nil {
		slog.Warn("Provider request budget preparation failed", "err", err)
		return nil, fmt.Errorf("prepare bounded provider request: %w", err)
	}
	return bounded, nil
}

// BuildModelReviewPrompt preserves source and required evidence while fitting optional context.
func BuildModelReviewPrompt(model Model, chunk diff.Chunk, minimumImportance int, required, optional string, policy reviewrules.Policy, maximumBytes int) (string, error) {
	prompt, err := renderChunkPrompt(chunk, minimumImportance, required+optional, policy)
	if err != nil {
		return "", err
	}
	size, err := modelReviewRequestBytes(model, prompt)
	if err != nil || size <= maximumBytes {
		return prompt, err
	}
	var data reviewrules.PromptData
	notice, err := renderPrompt(policy, "review.context_omitted", data)
	if err != nil {
		return "", err
	}
	best, err := renderChunkPrompt(chunk, minimumImportance, required+policy.WrapUntrusted("")+notice, policy)
	if err != nil {
		return "", err
	}
	size, err = modelReviewRequestBytes(model, best)
	if err != nil {
		return "", err
	}
	if size > maximumBytes {
		return "", &diff.PromptSizeError{Actual: size, Limit: maximumBytes}
	}
	lower, upper := 0, len(optional)
	for lower <= upper {
		middle := lower + (upper-lower)/2
		excerpt := policy.WrapUntrusted(policy.EscapeUntrusted(truncateUTF8(optional, middle)))
		candidate, err := renderChunkPrompt(chunk, minimumImportance, required+excerpt+notice, policy)
		if err != nil {
			return "", err
		}
		size, err := modelReviewRequestBytes(model, candidate)
		if err != nil {
			return "", err
		}
		if size <= maximumBytes {
			best, lower = candidate, middle+1
		} else {
			upper = middle - 1
		}
	}
	return best, nil
}

func modelReviewRequestBytes(model Model, prompt string) (int, error) {
	if sizer, ok := model.(RequestSizer); ok {
		size, err := sizer.ReviewRequestBytes(prompt)
		if err != nil {
			slog.Warn("Provider request measurement failed", "err", err)
			return 0, fmt.Errorf("measure provider request: %w", err)
		}
		return size, nil
	}
	return len(prompt), nil
}

func sourceScopePrompt(shortfall structuralShortfall, policy reviewrules.Policy) (string, error) {
	var data reviewrules.PromptData
	if !shortfall.present() {
		return renderPrompt(policy, "omissions.none", data)
	}
	data.Total = len(shortfall.Hunks)
	return renderPrompt(policy, "omissions.deferred", data)
}

package review

import (
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/diff"
)

// ConsolidationRequestSizer measures complete finding consolidation requests.
type ConsolidationRequestSizer interface {
	ConsolidateRequestBytes(string) (int, error)
}

// ReportRequestSizer measures complete final report requests.
type ReportRequestSizer interface {
	ReportRequestBytes(string) (int, error)
}

func consolidationPromptBytes(model Model, prompt string) (int, error) {
	if sizer, ok := model.(ConsolidationRequestSizer); ok {
		size, err := sizer.ConsolidateRequestBytes(prompt)
		if err != nil {
			slog.Warn("Consolidation request measurement failed", "err", err)
			return 0, fmt.Errorf("measure consolidation request: %w", err)
		}
		return size, nil
	}
	return len(prompt), nil
}

func reportPromptBytes(model Model, prompt string) (int, error) {
	if sizer, ok := model.(ReportRequestSizer); ok {
		size, err := sizer.ReportRequestBytes(prompt)
		if err != nil {
			slog.Warn("Report request measurement failed", "err", err)
			return 0, fmt.Errorf("measure report request: %w", err)
		}
		return size, nil
	}
	return len(prompt), nil
}

func boundStagePrompt(optional string, maximumBytes int, render func(string, bool) (string, error), measure func(string) (int, error)) (string, error) {
	complete, err := render(optional, false)
	if err != nil {
		return "", err
	}
	size, err := measure(complete)
	if err != nil {
		return "", err
	}
	if size <= maximumBytes {
		return complete, nil
	}
	best, err := render("", optional != "")
	if err != nil {
		return "", err
	}
	size, err = measure(best)
	if err != nil {
		return "", err
	}
	if size > maximumBytes {
		return "", &diff.PromptSizeError{Actual: size, Limit: maximumBytes}
	}
	lower, upper := 0, len(optional)
	for lower <= upper {
		middle := lower + (upper-lower)/2
		candidate, err := render(truncateUTF8(optional, middle), true)
		if err != nil {
			return "", err
		}
		size, err := measure(candidate)
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

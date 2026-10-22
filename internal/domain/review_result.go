package domain

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// UnmarshalReviewResult applies configured rule importance before publication can filter findings.
func UnmarshalReviewResult(data []byte, catalog reviewrules.Catalog, importance reviewrules.Importance) (ReviewResult, error) {
	result, err := decodeReviewResult(data)
	if err != nil {
		return ReviewResult{}, err
	}
	return validateReviewResult(result, catalog, importance)
}

// UnmarshalClassificationResult rejects findings before validating scope decisions.
func UnmarshalClassificationResult(data []byte) (ReviewResult, error) {
	result, err := decodeReviewResult(data)
	if err != nil {
		return ReviewResult{}, err
	}
	if len(result.Findings) != 0 {
		return ReviewResult{}, &FileAssessmentValidationError{Field: "findings"}
	}
	if err := result.Validate(); err != nil {
		return ReviewResult{}, err
	}
	return result, nil
}

func decodeReviewResult(data []byte) (ReviewResult, error) {
	var result ReviewResult
	if err := json.Unmarshal(data, &result); err != nil {
		slog.Warn("Review result decoding failed")
		return ReviewResult{}, fmt.Errorf("decode structured review: %w", err)
	}
	return result, nil
}

func validateReviewResult(result ReviewResult, catalog reviewrules.Catalog, importance reviewrules.Importance) (ReviewResult, error) {
	for index, finding := range result.Findings {
		score, err := catalog.FindingImportance(finding.RuleID, finding.Importance, importance)
		if err != nil {
			slog.Warn("Review rule importance validation failed", "rule_id", finding.RuleID)
			return ReviewResult{}, fmt.Errorf("apply review rule importance: %w", err)
		}
		result.Findings[index].Importance = score
		deduplication, err := catalog.FindingDeduplication(finding.RuleID)
		if err != nil {
			return ReviewResult{}, fmt.Errorf("apply review rule deduplication: %w", err)
		}
		result.Findings[index].Deduplication = deduplication
	}
	if err := result.Validate(); err != nil {
		return ReviewResult{}, err
	}
	return result, nil
}

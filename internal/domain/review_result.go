package domain

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// UnmarshalReviewResult applies configured rule importance before publication can filter findings.
func UnmarshalReviewResult(data []byte, importance reviewrules.Importance) (ReviewResult, error) {
	var result ReviewResult
	if err := json.Unmarshal(data, &result); err != nil {
		slog.Warn("Review result decoding failed")
		return ReviewResult{}, fmt.Errorf("decode structured review: %w", err)
	}
	for index, finding := range result.Findings {
		score, err := reviewrules.FindingImportance(finding.RuleID, finding.Importance, importance)
		if err != nil {
			slog.Warn("Review rule importance validation failed", "rule_id", finding.RuleID)
			return ReviewResult{}, fmt.Errorf("apply review rule importance: %w", err)
		}
		result.Findings[index].Importance = score
	}
	if err := result.Validate(); err != nil {
		return ReviewResult{}, err
	}
	return result, nil
}

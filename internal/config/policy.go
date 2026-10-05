package config

import (
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// LoadReviewPolicy requires explicit external rule and prompt files.
func LoadReviewPolicy(lookup LookupEnv) (reviewrules.Policy, error) {
	rules, configured := loadRequiredText(lookup, "REVIEW_RULES_FILE")
	if !configured {
		return reviewrules.Policy{}, fmt.Errorf("REVIEW_RULES_FILE is required")
	}
	prompts, configured := loadRequiredText(lookup, "REVIEW_PROMPTS_FILE")
	if !configured {
		return reviewrules.Policy{}, fmt.Errorf("REVIEW_PROMPTS_FILE is required")
	}
	policy, err := reviewrules.LoadPolicy(rules, prompts)
	if err != nil {
		slog.Warn("Review policy loading failed", "err", err)
		return reviewrules.Policy{}, fmt.Errorf("load review policy: %w", err)
	}
	return policy, nil
}

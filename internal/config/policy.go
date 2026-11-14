package config

import (
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/reviewrules"
)

var (
	errMissingReviewService   = errors.New("REVIEW_SERVICE_FILE is required")
	errMissingReviewTemplates = errors.New("REVIEW_TEMPLATES_DIR is required")
)

// LoadReviewPolicy requires explicit rule, prompt, service, and template paths.
func LoadReviewPolicy(lookup LookupEnv) (reviewrules.Policy, error) {
	rules, configured := loadRequiredText(lookup, "REVIEW_RULES_FILE")
	if !configured {
		return reviewrules.Policy{}, fmt.Errorf("REVIEW_RULES_FILE is required")
	}

	prompts, configured := loadRequiredText(lookup, "REVIEW_PROMPTS_FILE")
	if !configured {
		return reviewrules.Policy{}, fmt.Errorf("REVIEW_PROMPTS_FILE is required")
	}

	settings, configured := loadRequiredText(lookup, "REVIEW_SERVICE_FILE")
	if !configured {
		return reviewrules.Policy{}, errMissingReviewService
	}

	templates, configured := loadRequiredText(lookup, "REVIEW_TEMPLATES_DIR")
	if !configured {
		return reviewrules.Policy{}, errMissingReviewTemplates
	}

	policy, err := reviewrules.LoadPolicy(rules, prompts, settings, templates)
	if err != nil {
		slog.Warn("Review policy loading failed", "err", err)
		return reviewrules.Policy{}, fmt.Errorf("load review policy: %w", err)
	}

	return policy, nil
}

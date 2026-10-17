package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/quota"
)

// BudgetReportPolicy returns defaults for directly constructed configurations that omit delivery settings.
func (cfg Config) BudgetReportPolicy() quota.ReportPolicy {
	var omitted quota.ReportPolicy
	if cfg.ProviderBudgetReport == omitted {
		return quota.DefaultReportPolicy()
	}
	return cfg.ProviderBudgetReport
}

func loadBudgetReportPolicy(lookup LookupEnv) (quota.ReportPolicy, error) {
	policy := quota.DefaultReportPolicy()
	raw, present := lookup("PROVIDER_BUDGET_REPORT")
	if !present {
		return policy, nil
	}
	var values struct {
		Attempts       *int    `json:"attempts"`
		AttemptTimeout *string `json:"attempt_timeout"`
		Timeout        *string `json:"timeout"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&values); err != nil {
		return quota.ReportPolicy{}, errors.New("PROVIDER_BUDGET_REPORT must contain attempts, attempt_timeout, and timeout")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return quota.ReportPolicy{}, errors.New("PROVIDER_BUDGET_REPORT must contain one object")
	}
	if values.Attempts == nil || values.AttemptTimeout == nil || values.Timeout == nil {
		return quota.ReportPolicy{}, errors.New("PROVIDER_BUDGET_REPORT requires attempts, attempt_timeout, and timeout")
	}
	policy.Attempts = *values.Attempts
	attemptTimeout, err := time.ParseDuration(*values.AttemptTimeout)
	if err != nil {
		return quota.ReportPolicy{}, errors.New("PROVIDER_BUDGET_REPORT.attempt_timeout must be a duration")
	}
	policy.AttemptTimeout = attemptTimeout
	timeout, err := time.ParseDuration(*values.Timeout)
	if err != nil {
		return quota.ReportPolicy{}, errors.New("PROVIDER_BUDGET_REPORT.timeout must be a duration")
	}
	policy.Timeout = timeout
	if err := policy.Validate(); err != nil {
		slog.Error("validate provider budget report policy", "error", err)
		return quota.ReportPolicy{}, fmt.Errorf("PROVIDER_BUDGET_REPORT: %w", err)
	}
	return policy, nil
}

func loadDeliveryPolicies(lookup LookupEnv, cfg *Config) error {
	var err error
	cfg.Reassessment, err = loadReassessment(lookup)
	if err != nil {
		return err
	}
	cfg.ProviderBudgetReport, err = loadBudgetReportPolicy(lookup)
	return err
}

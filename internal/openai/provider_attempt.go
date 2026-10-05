package openai

import (
	"context"
	"errors"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/review"
)

type providerAttemptError struct {
	provider provider
	budget   budgetSnapshot
	cause    error
}

func (target provider) outputTokenLimit() int64 {
	if target.omitMaxOutputTokens {
		return 0
	}
	if target.maxOutputTokens != 0 {
		return target.maxOutputTokens
	}
	return config.MaximumOutputTokens
}

func (client *Client) recordProviderAttempt(ctx context.Context, target provider, budget budgetSnapshot, err error, fallback bool) {
	failure := &providerAttemptError{provider: target, budget: budget, cause: err}
	status := failure.ProviderStatus()
	if err == nil {
		status.Cause = review.ProviderSucceeded
	}
	attempt := review.ProviderAttempt{
		Status: status, API: target.api, ReasoningEffort: target.reasoningEffort,
		MaxOutputTokens: target.outputTokenLimit(), OutputCapOmitted: target.omitMaxOutputTokens,
		Completed: err == nil, Fallback: fallback, HTTPStatus: 0, ErrorCode: "", Count: 1, Sequence: 0,
	}
	var providerError *ProviderError
	if errors.As(err, &providerError) {
		attempt.HTTPStatus = providerError.StatusCode
		attempt.ErrorCode = providerError.Code
	}
	review.RecordProviderAttempt(ctx, attempt, client.reviewPolicy.ProviderDetails())
}

func (attempt *providerAttemptError) Error() string {
	return attempt.cause.Error()
}

func (attempt *providerAttemptError) Unwrap() error {
	return attempt.cause
}

func (attempt *providerAttemptError) ProviderStatus() review.ProviderStatus {
	status := review.ProviderStatus{
		ProviderID:    attempt.provider.id,
		Model:         attempt.provider.model,
		Cause:         review.ProviderRequestFailed,
		Used:          attempt.budget.used,
		Limit:         attempt.budget.limit,
		Remaining:     attempt.budget.remaining,
		QuotaKnown:    attempt.budget.known,
		TokenWindow:   attempt.budget.window,
		QuotaSnapshot: attempt.budget.accounting,
	}
	var admission *budgetAdmissionError
	if errors.As(attempt.cause, &admission) {
		status.Used = admission.snapshot.used
		status.Limit = admission.snapshot.limit
		status.Remaining = admission.snapshot.remaining
		status.QuotaKnown = admission.snapshot.known
		status.TokenWindow = admission.snapshot.window
		status.QuotaSnapshot = admission.snapshot.accounting
		if admission.DailyBudgetExhausted() {
			status.Cause = review.ProviderAppBudgetDenied
		} else {
			status.Cause = review.ProviderAppBudgetUnavailable
		}
		return status
	}
	var providerError *ProviderError
	if errors.As(attempt.cause, &providerError) {
		switch {
		case providerError.RateLimited():
			status.Cause = review.ProviderRateLimited
		case providerError.UsageExceeded():
			status.Cause = review.ProviderUsageExhausted
		}
	}
	return status
}

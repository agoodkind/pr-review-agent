package openai

import (
	"errors"

	"goodkind.io/pr-review-agent/internal/review"
)

type providerAttemptError struct {
	provider provider
	budget   budgetSnapshot
	cause    error
}

func (attempt *providerAttemptError) Error() string {
	return attempt.cause.Error()
}

func (attempt *providerAttemptError) Unwrap() error {
	return attempt.cause
}

func (attempt *providerAttemptError) ProviderStatus() review.ProviderStatus {
	status := review.ProviderStatus{
		ProviderID: attempt.provider.id,
		Model:      attempt.provider.model,
		Cause:      review.ProviderRequestFailed,
		Used:       attempt.budget.used,
		Limit:      attempt.budget.limit,
		Remaining:  attempt.budget.remaining,
		QuotaKnown: attempt.budget.known,
	}
	var admission *budgetAdmissionError
	if errors.As(attempt.cause, &admission) {
		status.Used = admission.snapshot.used
		status.Limit = admission.snapshot.limit
		status.Remaining = admission.snapshot.remaining
		status.QuotaKnown = admission.snapshot.known
		if admission.DailyBudgetExhausted() {
			status.Cause = review.ProviderAppBudgetDenied
		} else {
			status.Cause = review.ProviderAppBudgetUnavailable
		}
		return status
	}
	var providerError *ProviderError
	if errors.As(attempt.cause, &providerError) && providerError.UsageExceeded() {
		status.Cause = review.ProviderUsageExhausted
	}
	return status
}

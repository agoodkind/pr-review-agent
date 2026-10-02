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
		Cause:      "",
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
			status.Cause = "The app denied the request; the provider API was not called."
		} else {
			status.Cause = "The app could not check its quota; the provider API was not called."
		}
		return status
	}
	var providerError *ProviderError
	if errors.As(attempt.cause, &providerError) && providerError.UsageExceeded() {
		status.Cause = "The provider API reported no remaining usage."
	} else {
		status.Cause = "The provider API request failed."
	}
	return status
}

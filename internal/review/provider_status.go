package review

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ProviderFailureCause identifies the boundary that rejected a model request.
type ProviderFailureCause string

const (
	// ProviderAppBudgetDenied means the app rejected a request before calling the provider.
	ProviderAppBudgetDenied ProviderFailureCause = "app_budget_denied"
	// ProviderAppBudgetUnavailable means the app could not check its configured quota.
	ProviderAppBudgetUnavailable ProviderFailureCause = "app_budget_unavailable"
	// ProviderUsageExhausted means the provider reported no remaining usage.
	ProviderUsageExhausted ProviderFailureCause = "provider_usage_exhausted"
	// ProviderRequestFailed means the provider request failed for another reason.
	ProviderRequestFailed ProviderFailureCause = "provider_request_failed"
)

// ProviderStatus records a configured model's failed attempt without provider-supplied text.
type ProviderStatus struct {
	ProviderID string
	Model      string
	Cause      ProviderFailureCause
	Used       int64
	Limit      int64
	Remaining  int64
	QuotaKnown bool
}

func addAttemptedModels(summary *Summary, statuses []ProviderStatus) {
	for _, status := range statuses {
		if !slices.Contains(summary.Models, status.Model) {
			summary.Models = append(summary.Models, status.Model)
		}
	}
}

func providerStatuses(failures []chunkFailure) []ProviderStatus {
	statuses := make([]ProviderStatus, 0)
	for _, failure := range failures {
		collectProviderStatuses(failure.err, &statuses)
	}
	return statuses
}

func collectProviderStatuses(err error, statuses *[]ProviderStatus) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			collectProviderStatuses(cause, statuses)
		}
		return
	}
	if reported, ok := err.(interface{ ProviderStatus() ProviderStatus }); ok {
		status := reported.ProviderStatus()
		for index, previous := range *statuses {
			if previous.ProviderID == status.ProviderID && previous.Model == status.Model && previous.Cause == status.Cause {
				if status.QuotaKnown && (!previous.QuotaKnown || status.Used > previous.Used) {
					(*statuses)[index] = status
				}
				return
			}
		}
		*statuses = append(*statuses, status)
		return
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		collectProviderStatuses(wrapped.Unwrap(), statuses)
	}
}

func renderProviderStatuses(statuses []ProviderStatus) string {
	if len(statuses) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("#### Provider attempts\n\n| Provider | Model | App quota | Result |\n| --- | --- | --- | --- |\n")
	for _, status := range statuses {
		quota := "No app limit"
		switch {
		case status.Limit == 0:
		case status.QuotaKnown:
			quota = fmt.Sprintf("%s / %s (%s left)", formatTokenCount(status.Used), formatTokenCount(status.Limit), formatTokenCount(status.Remaining))
		case status.Limit > 0:
			quota = "Unknown / " + formatTokenCount(status.Limit)
		}
		fmt.Fprintf(&builder, "| %s | %s | %s | %s |\n",
			usageModelCell(status.ProviderID), usageModelCell(status.Model), quota, providerCauseLabel(status.Cause))
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

func providerCauseLabel(cause ProviderFailureCause) string {
	switch cause {
	case ProviderAppBudgetDenied:
		return "App denied request; API not called"
	case ProviderAppBudgetUnavailable:
		return "App quota check failed; API not called"
	case ProviderUsageExhausted:
		return "Provider reported no remaining usage"
	case ProviderRequestFailed:
		return "Provider request failed"
	default:
		return "Provider request failed"
	}
}

func formatTokenCount(count int64) string {
	digits := strconv.FormatInt(count, 10)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}

package review

import (
	"reflect"
	"slices"
	"strconv"
	"time"

	"goodkind.io/pr-review-agent/internal/quota"
)

// ProviderFailureCause identifies the boundary that rejected a model request.
type ProviderFailureCause string

const (
	// ProviderSucceeded means the API completed the requested response.
	ProviderSucceeded ProviderFailureCause = "provider_succeeded"
	// ProviderAppBudgetDenied means the app rejected a request before calling the provider.
	ProviderAppBudgetDenied ProviderFailureCause = "app_budget_denied"
	// ProviderAppBudgetUnavailable means the app could not check its configured quota.
	ProviderAppBudgetUnavailable ProviderFailureCause = "app_budget_unavailable"
	// ProviderUsageExhausted means the provider reported no remaining usage.
	ProviderUsageExhausted ProviderFailureCause = "provider_usage_exhausted"
	// ProviderRateLimited means the provider throttled a request.
	ProviderRateLimited ProviderFailureCause = "provider_rate_limited"
	// ProviderRequestFailed means the provider request failed for another reason.
	ProviderRequestFailed ProviderFailureCause = "provider_request_failed"
)

// ProviderStatus records a configured model's failed attempt without provider-supplied text.
type ProviderStatus struct {
	ProviderID    string
	Model         string
	Cause         ProviderFailureCause
	Used          int64
	Limit         int64
	Remaining     int64
	QuotaKnown    bool
	TokenWindow   quota.Window
	QuotaSnapshot quota.Snapshot
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
			if previous.ProviderID == status.ProviderID && previous.Model == status.Model &&
				previous.Cause == status.Cause &&
				sameQuotaInterval(previous, status) {
				if status.QuotaKnown &&
					(!previous.QuotaKnown ||
						status.Used > previous.Used ||
						status.Used == previous.Used &&
							status.QuotaSnapshot.Bounds.EndMS > previous.QuotaSnapshot.Bounds.EndMS) {
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

func sameQuotaInterval(previous, current ProviderStatus) bool {
	if previous.TokenWindow != current.TokenWindow {
		return false
	}

	if current.TokenWindow.Mode == quota.Rolling {
		return true
	}

	return previous.QuotaSnapshot.Bounds == current.QuotaSnapshot.Bounds
}

func renderProviderStatuses(statuses []ProviderStatus) (string, error) {
	var state renderState

	if len(statuses) == 0 {
		return state.finish("")
	}

	rows := make([]providerStatusOutput, 0, len(statuses))
	for _, status := range statuses {
		label := state.capture(
			renderOutputTemplate("output.provider-window", reflect.ValueOf(status.TokenWindow)),
		)
		rows = append(
			rows,
			providerStatusOutput{
				Status:    status,
				Provider:  state.capture(usageModelCell(status.ProviderID)),
				Model:     state.capture(usageModelCell(status.Model)),
				Used:      tokenCountGroups(status.Used),
				Limit:     tokenCountGroups(status.Limit),
				Remaining: tokenCountGroups(status.Remaining),
				Cause:     state.capture(providerCauseLabel(status.Cause)),
				Window:    state.capture(usageModelCell(label)),
				HistoryStart: time.UnixMilli(status.QuotaSnapshot.HistoryStartMS).UTC().Format(
					time.RFC3339,
				),
				AvailableAt: time.UnixMilli(status.QuotaSnapshot.AvailableAtMS).UTC().Format(
					time.RFC3339,
				),
			},
		)
	}

	return state.finish(
		state.capture(renderOutputTemplate("output.provider-statuses", reflect.ValueOf(rows))),
	)
}

type providerStatusOutput struct {
	Status       ProviderStatus
	Provider     string
	Model        string
	Used         []string
	Limit        []string
	Remaining    []string
	Cause        string
	Window       string
	HistoryStart string
	AvailableAt  string
}

func providerCauseLabel(cause ProviderFailureCause) (string, error) {
	var state renderState

	return state.finish(
		state.capture(renderOutputTemplate("output.provider-cause", reflect.ValueOf(cause))),
	)
}

func tokenCountGroups(count int64) []string {
	digits := strconv.FormatInt(count, 10)
	var groups []string

	for len(digits) > 3 {
		index := len(digits) - 3
		groups = append(groups, digits[index:])
		digits = digits[:index]
	}

	groups = append(groups, digits)
	slices.Reverse(groups)

	return groups
}

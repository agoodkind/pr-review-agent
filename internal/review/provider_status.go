package review

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ProviderStatus records a configured model's failed attempt without provider-supplied text.
type ProviderStatus struct {
	ProviderID string
	Model      string
	Cause      string
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
	for _, status := range statuses {
		fmt.Fprintf(&builder, "- %s uses %s. ", codeSpan(status.ProviderID), codeSpan(status.Model))
		switch {
		case status.Limit == 0:
			builder.WriteString("The app has no configured token limit. ")
		case status.QuotaKnown:
			fmt.Fprintf(&builder, "The app counted %s of %s tokens today; %s remain. ", formatTokenCount(status.Used), formatTokenCount(status.Limit), formatTokenCount(status.Remaining))
		default:
			fmt.Fprintf(&builder, "The app's limit is %s tokens; current usage is unavailable. ", formatTokenCount(status.Limit))
		}
		builder.WriteString(status.Cause)
		builder.WriteByte('\n')
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

func formatTokenCount(count int64) string {
	digits := strconv.FormatInt(count, 10)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}

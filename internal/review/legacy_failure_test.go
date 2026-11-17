package review

import (
	"strings"
	"testing"
)

func TestStripLegacyFailureProsePreservesSavedSections(t *testing.T) {
	body := "## Review\n\nReview failed while loading the pull request.\n\n" +
		"The review service encountered an unexpected error (HTTP 500) and could not complete the review.\n\n" +
		"The cause is recorded in this service's log, under run identifier `old`.\n\n" +
		"<!-- pr-review-agent:verdict-section:start -->\n\nSaved finding.\n\n" +
		"<!-- pr-review-agent:verdict-section:end -->\n\n" +
		"<details>\n<summary>Review details</summary>\n\nSaved statistics.\n</details>"

	got, err := stripLegacyFailureProse(body)
	if err != nil || strings.Contains(got, "old") || strings.Contains(got, "Review failed") ||
		!strings.Contains(got, "Saved finding.") || !strings.Contains(got, "Saved statistics.") {
		t.Fatalf(
			"Cleanup did not retain the saved findings and statistics after removing the old failure: %q, %v",
			got,
			err,
		)
	}
}

func TestStripLegacyFailureProsePreservesUnrecognizedReview(t *testing.T) {
	body := "## Review\n\nA saved review explains the provider error.\n\n" +
		"The review service encountered an unexpected error and could not complete the review."
	got, err := stripLegacyFailureProse(body)
	if err != nil || got != body {
		t.Fatalf("Cleanup did not preserve the saved review: %q, %v", got, err)
	}
}

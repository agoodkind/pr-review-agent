package review

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
)

func TestFailureReplacesSavedReviewProgress(t *testing.T) {
	const head = "a3c4f1c"
	const details = "<details>\n<summary>Review details</summary>\n\nSaved statistics.\n</details>"
	current, err := RenderStartedBody(domain.HeadSHA(head))
	if err != nil {
		t.Fatal(err)
	}

	for _, started := range []string{"## Review\n\nReviewing `" + head + "`.", current} {
		body, err := renderMaintenanceFailure(started+"\n\n"+details, domain.AssessmentOutcome{})
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(body, head) || !strings.Contains(body, details) ||
			!strings.Contains(body, "PR Review Agent could not finish this review") {
			t.Fatalf(
				"The failure notice must replace progress and preserve saved details: %q",
				body,
			)
		}
	}
}

func TestSavedIncompleteCoverageWithholdsApproval(t *testing.T) {
	current, err := renderOutputText(unreviewedHeadReason)
	if err != nil {
		t.Fatal(err)
	}

	for _, reason := range []string{legacyUnreviewedHeadReason, current} {
		snapshot := parseAssessmentSnapshot("## Review\n\n" + reason)
		if snapshot.legacyWholeHead {
			t.Fatalf(
				"The incomplete coverage notice must keep the commit marked incomplete: %q",
				reason,
			)
		}
	}
}

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

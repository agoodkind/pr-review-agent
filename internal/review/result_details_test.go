package review_test

import (
	"html"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/review"
)

func TestReviewDetailsRenderReturnedResult(t *testing.T) {
	const failure = "file assessment validation failed: code=assessment_persistence_limit field= observed=83647 limit=48000"

	for _, result := range []string{"", failure} {
		var summary review.Summary
		summary.Result = result
		body := review.RenderDetails(summary)
		expected := result
		if expected == "" {
			expected = "null"
		}

		if !strings.Contains(body, "| `result` | `"+expected+"` |") {
			t.Fatalf("Review details omitted the returned result: %s", body)
		}
	}
}

func TestReviewDetailsPreserveMultilineErrorText(t *testing.T) {
	const failure = "first error: `value` | <input>\nsecond error: &details"
	var summary review.Summary
	summary.Result = failure
	body := review.RenderDetails(summary)
	_, encoded, found := strings.Cut(body, "| `result` | <pre>")
	if !found {
		t.Fatalf("Review details omitted the multiline error: %s", body)
	}

	encoded, _, found = strings.Cut(encoded, "</pre> |")
	if !found || html.UnescapeString(encoded) != failure || strings.Contains(encoded, "\n") {
		t.Fatalf("Review details changed the error text: %q", encoded)
	}
}

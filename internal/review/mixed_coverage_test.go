package review_test

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestUnreadChangesRetainProviderDiagnosticsInsideDetails(t *testing.T) {
	notice := "Review stopped: the app exhausted its configured token limit.\n\n" +
		"GitHub supplied no patch for generated.txt."
	statuses := []review.ProviderStatus{
		{
			ProviderID: "primary", Model: "primary-model", Cause: review.ProviderAppBudgetDenied,
			Used: 101, Limit: 100, Remaining: 0, QuotaKnown: true,
		},
		{ProviderID: "secondary", Model: "secondary-model", Cause: review.ProviderRequestFailed},
	}
	body := review.RenderUnreadableBody(review.Summary{
		Head: domain.HeadSHA(testHeadSHA), Decision: domain.ReviewDecisionComment,
	}, notice, statuses...)
	visible, details, found := strings.Cut(body, "<details>")
	if !found {
		t.Fatal("review diagnostics are not collapsed")
	}
	for _, expected := range []string{"app exhausted", "GitHub supplied no patch"} {
		if !strings.Contains(visible, expected) {
			t.Fatalf("visible comment omits %q", expected)
		}
	}
	if strings.Contains(visible, "primary-model") || strings.Contains(visible, "Provider attempts") {
		t.Fatal("provider diagnostics are visible outside the collapsed section")
	}
	for _, expected := range []string{"primary-model", "secondary-model", "101 / 100 (0 left)", "App denied request; API not called", "Provider request failed"} {
		if !strings.Contains(details, expected) {
			t.Fatalf("collapsed diagnostics omit %q", expected)
		}
	}
}

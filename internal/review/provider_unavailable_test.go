package review_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/openai"
)

func TestProviderUnavailableIsReportedWithoutRetryAdvice(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "HTTP failure", err: &openai.ProviderError{StatusCode: http.StatusBadGateway}},
		{name: "flattened upstream failure", err: &openai.ProviderError{
			StatusCode: http.StatusBadRequest,
			Message:    "upstream call failed: upstream_status=502",
		}},
		{name: "broken stream", err: &openai.StreamError{
			Model: "test-model", Cause: errors.New("connection reset"), Provider: nil,
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assertProviderUnavailable(t, testCase.err)
		})
	}
}

func assertProviderUnavailable(t *testing.T, providerError error) {
	t.Helper()
	model := &failThenSucceedModel{err: providerError}
	fixture := newServiceFixture(t, serviceFixtureOptions{model: model})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := "The model provider is unavailable. Wait for it to recover."
	body := failureSummaryComment(t, fixture)
	if !strings.Contains(body, want) {
		t.Fatalf("summary comment = %q, want %q", body, want)
	}
	recovery := "The model provider must recover before the next push can review what remains."
	if !strings.Contains(body, recovery) {
		t.Fatalf("summary comment = %q, want %q", body, recovery)
	}

	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok || !strings.Contains(fmt.Sprint(output["title"]), want) {
		t.Fatalf("check title = %v, want %q", output["title"], want)
	}
	if model.calls != 1 {
		t.Fatalf("model calls = %d, want 1 without retry", model.calls)
	}
	if len(decodedSummaryState(t, fixture).Pending) != 1 {
		t.Fatalf("pending chunks = %v, want the failed chunk retained", decodedSummaryState(t, fixture).Pending)
	}
	if len(fixture.state.submittedReviews) != 0 {
		t.Fatalf("submitted reviews = %v, want none during an outage", fixture.state.submittedReviews)
	}
}

func TestAStreamDeadlineKeepsTheTimeoutClassification(t *testing.T) {
	model := &failThenSucceedModel{err: &openai.StreamError{
		Model: "test-model", Cause: context.DeadlineExceeded, Provider: nil,
	}}
	fixture := newServiceFixture(t, serviceFixtureOptions{model: model})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body := failureSummaryComment(t, fixture)
	if !strings.Contains(body, "Review stopped: it ran out of time.") {
		t.Fatalf("summary comment = %q, want the timeout classification", body)
	}
	if strings.Contains(body, "provider is unavailable") {
		t.Fatalf("summary comment misclassifies a deadline: %q", body)
	}
}

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
	if strings.Contains(body, "The next push reviews") {
		t.Fatalf("summary comment recommends another request during an outage: %q", body)
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

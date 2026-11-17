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

func TestProviderUnavailableIsReportedWithRecoveryAndRetryAdvice(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "HTTP failure", err: &openai.ProviderError{StatusCode: http.StatusBadGateway}},
		{
			name: "flattened upstream failure",
			err: &openai.ProviderError{
				StatusCode: http.StatusBadRequest,
				Type:       "invalid_request_error",
				Code:       "upstream_failed",
				Message:    "Upstream call failed: Upstream status 502",
			},
		},
		{
			name: "broken stream",
			err: &openai.StreamError{
				Model:    "test-model",
				Cause:    errors.New("connection reset"),
				Provider: nil,
			},
		},
	}
	for _, testCase := range cases {
		t.Run(
			testCase.name,
			func(t *testing.T) {
				assertProviderUnavailable(t, testCase.err)
			},
		)
	}
}

func TestRequestTextCannotClaimProviderUnavailable(t *testing.T) {
	model := &failThenSucceedModel{
		err: &openai.ProviderError{
			StatusCode: http.StatusBadRequest,
			Type:       "invalid_request_error",
			Code:       "upstream_failed",
			Message:    "request rejected: prompt contains upstream_status=502",
		},
	}
	fixture := newServiceFixture(t, serviceFixtureOptions{model: model})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if strings.Contains(failureSummaryComment(t, fixture), "provider is unavailable") {
		t.Fatalf(
			"summary comment trusts request text as gateway metadata: %q",
			failureSummaryComment(t, fixture),
		)
	}
}

func assertProviderUnavailable(t *testing.T, providerError error) {
	t.Helper()

	model := &failThenSucceedModel{err: providerError}
	fixture := newServiceFixture(t, serviceFixtureOptions{model: model})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := "PR Review Agent could not finish this review and has not scheduled a retry."

	body := failureSummaryComment(t, fixture)
	if !strings.Contains(body, want) {
		t.Fatalf("summary comment = %q, want %q", body, want)
	}

	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok ||
		!strings.Contains(
			fmt.Sprint(output["title"]),
			"Apply `test-review-agent-rerun` to retry.",
		) {
		t.Fatalf("check title = %v, want the same-head retry action", output["title"])
	}

	if model.calls != 1 {
		t.Fatalf("model calls = %d, want 1 without retry", model.calls)
	}

	if len(decodedSummaryState(t, fixture).Pending) != 1 {
		t.Fatalf(
			"pending chunks = %v, want the failed chunk retained",
			decodedSummaryState(t, fixture).Pending,
		)
	}

	if len(fixture.state.submittedReviews) != 0 {
		t.Fatalf(
			"submitted reviews = %v, want none during an outage",
			fixture.state.submittedReviews,
		)
	}
}

func TestAStreamDeadlineKeepsTheTimeoutClassification(t *testing.T) {
	model := &failThenSucceedModel{
		err: &openai.StreamError{
			Model:    "test-model",
			Cause:    context.DeadlineExceeded,
			Provider: nil,
		},
	}
	fixture := newServiceFixture(t, serviceFixtureOptions{model: model})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body := failureSummaryComment(t, fixture)
	if !strings.Contains(
		body,
		"PR Review Agent could not finish this review and has not scheduled a retry.",
	) {
		t.Fatalf(
			"The summary comment %q does not say that the agent has not scheduled a retry.",
			body,
		)
	}

	if strings.Contains(body, "provider is unavailable") {
		t.Fatalf("summary comment misclassifies a deadline: %q", body)
	}
}

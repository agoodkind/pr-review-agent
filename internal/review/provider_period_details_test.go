package review_test

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/review"
)

func TestRenderDetailsShowsRecordedRequestsWithoutCumulativeUsage(t *testing.T) {
	ctx, recorder := review.WithUsageRecorder(t.Context())

	for _, attempt := range []review.ProviderAttempt{
		{
			Status:    review.ProviderStatus{ProviderID: "z-first|provider", Model: "first"},
			ErrorCode: "rate`limited|raw",
			Count:     1,
		},
		{
			Status:    review.ProviderStatus{ProviderID: "a-second", Model: "requested"},
			Completed: true,
			Count:     1,
		},
	} {
		review.RecordProviderAttempt(ctx, attempt, nil)
	}

	review.RecordModelUsage(
		ctx,
		review.ModelUsage{ProviderID: "z-first|provider", RequestedModel: "first", Requests: 1},
	)

	for _, model := range []string{"reported-one", "reported-two"} {
		review.RecordModelUsage(
			ctx,
			review.ModelUsage{
				ProviderID:       "a-second",
				RequestedModel:   "requested",
				Model:            model,
				Requests:         1,
				ReportedRequests: 1,
				InputTokens:      11,
				OutputTokens:     7,
				TotalTokens:      18,
			},
		)
	}

	usage := recorder.Summary()
	usage.PeriodUsage = []review.ProviderPeriodUsage{
		{
			ProviderID:       "a-second",
			RequestedModel:   "requested",
			Available:        true,
			TotalTokens:      987654321,
			EstimatedCostUSD: 123.456789,
			CostComplete:     true,
		},
	}
	body := review.RenderDetails(review.Summary{Usage: usage})

	for _, expected := range []string{
		"| `providers` | `z-first\\|provider`: ``rate`limited\\|raw``<br>`a-second`: `36` |",
		"| `requests` | `3` |",
		"| `reported_requests` | `2` |",
		"| `input_tokens` | `22` |",
		"| `output_tokens` | `14` |",
		"| `total_tokens` | `36` |",
		"| `estimated_cost_usd` | `null` |",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("Review details omitted %q.\n%s", expected, body)
		}
	}

	if strings.Count(body, "<details>") != 1 || strings.Contains(body, "<details open") ||
		strings.Count(body, "<summary>Review details</summary>") != 1 ||
		strings.Count(body, "| --- | --- |") != 1 {
		t.Fatalf("Review details must contain one collapsed block and one table.\n%s", body)
	}

	for _, removed := range []string{
		"987654321",
		"123.456789",
		"Period usage",
		"Provider details",
		"reported-one",
		"reported-two",
	} {
		if strings.Contains(body, removed) {
			t.Fatalf("Review details included removed display %q.\n%s", removed, body)
		}
	}
}

package review_test

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/review"
)

func TestUsageDetailsPreserveCacheWritesAndActualServiceTiers(t *testing.T) {
	ctx, recorder := review.WithUsageRecorder(t.Context())
	for _, tier := range []string{"default", "flex"} {
		review.RecordModelUsage(ctx, review.ModelUsage{
			ProviderID: "provider", RequestedModel: "model", Model: "model", ServiceTier: tier,
			Requests: 1, ReportedRequests: 1, CacheWriteTokens: 200_000,
			Priced: true, EstimatedCacheWriteCostUSD: 0.05, EstimatedCostUSD: 0.258,
		})
	}
	summary := recorder.Summary()
	if len(summary.Models) != 2 || summary.Models[0].ServiceTier != "default" || summary.Models[1].ServiceTier != "flex" {
		t.Fatalf("usage combined requests with different service tiers: %+v", summary.Models)
	}
	body := review.RenderDetails(review.Summary{Usage: summary})
	for _, expected := range []string{"| Cache write tokens | `400000` |", "| Reported service tiers | `default`, `flex` |", "| Estimated cache write cost | $0.10000000 |", "| Estimated total cost | $0.51600000 |"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("review diagnostics omitted %q", expected)
		}
	}
}

func TestUsageDetailsDoNotPriceUnpricedCacheWritesAsZero(t *testing.T) {
	for _, scenario := range []struct {
		name            string
		unpricedWrites  int64
		pricedWrites    int64
		pricedWriteCost float64
		expected        string
	}{
		{name: "only unpriced writes", unpricedWrites: 200_000, expected: "unknown"},
		{name: "partial write estimate", unpricedWrites: 200_000, pricedWrites: 200_000, pricedWriteCost: 0.05, expected: "unknown"},
		{name: "unpriced request without writes", pricedWrites: 200_000, pricedWriteCost: 0.05, expected: "$0.05000000"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, recorder := review.WithUsageRecorder(t.Context())
			review.RecordModelUsage(ctx, review.ModelUsage{
				RequestedModel: "priced", Model: "priced", Requests: 1, ReportedRequests: 1,
				Priced: true, CacheWriteTokens: scenario.pricedWrites,
				InputTokens: scenario.pricedWrites, TotalTokens: scenario.pricedWrites,
				EstimatedCacheWriteCostUSD: scenario.pricedWriteCost, EstimatedCostUSD: scenario.pricedWriteCost,
			})
			review.RecordModelUsage(ctx, review.ModelUsage{
				RequestedModel: "unpriced", Model: "unpriced", Requests: 1, ReportedRequests: 1,
				CacheWriteTokens: scenario.unpricedWrites,
				InputTokens:      scenario.unpricedWrites, TotalTokens: scenario.unpricedWrites,
			})
			body := review.RenderDetails(review.Summary{Usage: recorder.Summary()})
			if !strings.Contains(body, "| Estimated cache write cost | "+scenario.expected+" |") {
				t.Fatalf("review diagnostics misstated cache write cost: %s", body)
			}
		})
	}
}

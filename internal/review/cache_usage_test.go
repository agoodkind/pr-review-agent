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

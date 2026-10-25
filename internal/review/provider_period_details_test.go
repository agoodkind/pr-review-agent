package review_test

import (
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestRenderDetailsReportsProviderPeriodCoverage(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		tokens    int64
		cost      float64
		available bool
		history   bool
		priced    int
		complete  bool
		expected  string
		coverage  string
	}{
		{name: "millions", tokens: 1_200_000, cost: 0.24, available: true, history: true, priced: 2, complete: true, expected: "1.2m tokens / ~$0.24"},
		{name: "thousands", tokens: 1_000, cost: 0.000024, available: true, history: true, priced: 2, complete: true, expected: "1k tokens / ~$0.000024"},
		{name: "billions", tokens: 2_000_000_000, cost: 25.12, available: true, history: true, priced: 2, complete: true, expected: "2b tokens / ~$25.12"},
		{name: "unit boundary", tokens: 999_999, cost: 1, available: true, history: true, priced: 2, complete: true, expected: "999.999k tokens / ~$1.00"},
		{name: "partial history", tokens: 1_200_000, cost: 0.24, available: true, priced: 2, complete: true, expected: "1.2m tokens / ~$0.24", coverage: "Partial history from 2026-10-24 12:00:00 UTC"},
		{name: "partial prices", tokens: 1_200_000, cost: 0.12, available: true, history: true, priced: 1, expected: "1.2m tokens / ~$0.12", coverage: "Cost is incomplete: prices cover 1 of 2 requests."},
		{name: "unpriced", tokens: 1_000, available: true, history: true, expected: "1k tokens / cost unavailable"},
		{name: "known zero", available: true, history: true, complete: true, expected: "0 tokens / ~$0.00"},
		{name: "unavailable", expected: "-->unavailable"},
		{name: "not configured", expected: "-->period not configured"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var period review.ProviderPeriodUsage
			period.ProviderID, period.RequestedModel = "provider", "model"
			if scenario.name != "not configured" {
				period.Window.Mode, period.Window.Duration = quota.Rolling, "24h"
			}
			period.Available, period.HistoryComplete, period.CostComplete = scenario.available, scenario.history, scenario.complete
			period.TotalTokens, period.EstimatedCostUSD = scenario.tokens, scenario.cost
			period.PricedRequests, period.ReportedRequests = scenario.priced, 2
			period.Bounds.StartMS = time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC).UnixMilli()
			period.Bounds.EndMS = time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC).UnixMilli()
			period.AsOfMS = time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC).UnixMilli()
			period.HistoryStartMS = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC).UnixMilli()
			var summary review.Summary
			summary.Usage.ProviderColumns = []reviewrules.ProviderDetailColumn{reviewrules.ProviderIdentityColumn, reviewrules.ProviderModelsColumn, reviewrules.ProviderPeriodUsageColumn}
			summary.Usage.PeriodUsage = []review.ProviderPeriodUsage{period}
			body := review.RenderDetails(summary)
			if !strings.Contains(body, scenario.expected) || !strings.Contains(body, scenario.coverage) {
				t.Fatalf("The provider period cell does not report the expected measurement and coverage.\n%s", body)
			}
			if scenario.available && (!strings.Contains(body, "2026-10-24 00:00:00 UTC to 2026-10-25 00:00:00 UTC") || !strings.Contains(body, "Captured 2026-10-24 18:00:00 UTC")) {
				t.Fatalf("The provider period cell does not identify its range and capture time.\n%s", body)
			}
			if !scenario.available && (strings.Contains(body, "0 tokens") || strings.Contains(body, "~$0")) {
				t.Fatalf("The unavailable provider period incorrectly reports zero usage.\n%s", body)
			}
			if scenario.name == "millions" {
				t.Log(body[strings.Index(body, "#### Provider details"):])
			}
		})
	}
}

func TestRenderDetailsSeparatesProviderPeriodsFromReviewCosts(t *testing.T) {
	ctx, recorder := review.WithUsageRecorder(t.Context())
	review.RecordModelUsage(ctx, review.ModelUsage{ProviderID: "provider", RequestedModel: "model", Model: "model", Requests: 1, ReportedRequests: 1, InputTokens: 10, TotalTokens: 10, Priced: true, EstimatedCostUSD: 0.02})
	var attempt review.ProviderAttempt
	attempt.Status.ProviderID, attempt.Status.Model, attempt.Count = "provider", "model", 1
	var period review.ProviderPeriodUsage
	period.ProviderID, period.RequestedModel = "provider", "model"
	period.Available, period.CostComplete, period.HistoryComplete = true, true, true
	period.TotalTokens, period.EstimatedCostUSD = 1_200_000, 0.24
	var summary review.Summary
	summary.Usage = recorder.Summary()
	summary.Usage.ProviderAttempts = []review.ProviderAttempt{attempt}
	summary.Usage.ProviderColumns = []reviewrules.ProviderDetailColumn{reviewrules.ProviderTokensColumn, reviewrules.ProviderCostColumn, reviewrules.ProviderPeriodUsageColumn}
	summary.Usage.PeriodUsage = []review.ProviderPeriodUsage{period}
	body := review.RenderDetails(summary)
	for _, expected := range []string{"Total 10", "$0.02000000", "1.2m tokens / ~$0.24", "| Estimated total cost | $0.02000000 |", "across all models for each provider", "do not represent account charges"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("The rendered review does not separate review and period usage for %q.\n%s", expected, body)
		}
	}
	summary.Usage.ProviderColumns = []reviewrules.ProviderDetailColumn{reviewrules.ProviderCostColumn}
	disabled := review.RenderDetails(summary)
	if strings.Contains(disabled, "Period usage") || strings.Contains(disabled, "1.2m") {
		t.Fatalf("The renderer included the disabled period column.\n%s", disabled)
	}
}

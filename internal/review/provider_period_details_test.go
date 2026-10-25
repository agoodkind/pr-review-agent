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
		{name: "unavailable", expected: "Rolling `24h`<br>unavailable"},
		{name: "not configured", expected: "-->token quota periods not configured"},
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

func TestRenderDetailsIncludesEveryProviderQuotaWindow(t *testing.T) {
	captured := time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)
	var summary review.Summary
	summary.Usage.ProviderColumns = []reviewrules.ProviderDetailColumn{reviewrules.ProviderIdentityColumn, reviewrules.ProviderPeriodUsageColumn}
	for index, window := range []quota.Window{
		{Mode: quota.Rolling, Duration: "24h", Anchor: "", Schedule: "", Timezone: ""},
		{Mode: quota.Fixed, Duration: "168h", Anchor: "2026-10-01T00:00:00Z", Schedule: "", Timezone: ""},
		{Mode: quota.Cron, Duration: "", Anchor: "", Schedule: "0 0 1 * *", Timezone: "America/Los_Angeles"},
		{Mode: quota.Rolling, Duration: "1h", Anchor: "", Schedule: "", Timezone: ""},
	} {
		bounds, err := window.Bounds(captured)
		if err != nil {
			t.Fatal(err)
		}
		var period review.ProviderPeriodUsage
		period.ProviderID, period.RequestedModel = "provider", "model"
		period.Window, period.Bounds, period.AsOfMS = window, bounds, captured.UnixMilli()
		period.Available = index < 3
		period.HistoryComplete, period.CostComplete = true, true
		period.TotalTokens = int64(index+1) * 1_000_000
		period.EstimatedCostUSD = float64(index+1) / 10
		summary.Usage.PeriodUsage = append(summary.Usage.PeriodUsage, period)
	}
	var other review.ProviderPeriodUsage
	other.ProviderID, other.RequestedModel = "other", "model"
	summary.Usage.PeriodUsage = append(summary.Usage.PeriodUsage, other)
	body := review.RenderDetails(summary)
	for _, expected := range []string{
		"Rolling `24h`<br>1m tokens / ~$0.10",
		"Fixed `168h`<br>2m tokens / ~$0.20",
		"Cron `0 0 1 * *` `America/Los_Angeles`<br>3m tokens / ~$0.30",
		"Rolling `1h`<br>unavailable",
		"2026-10-24 18:00:00 UTC to 2026-10-25 18:00:00 UTC",
		"2026-10-22 00:00:00 UTC to 2026-10-29 00:00:00 UTC",
		"2026-10-01 07:00:00 UTC to 2026-11-01 07:00:00 UTC",
		"| `other` | <!-- pr-review-agent:period-usage:v1 provider=b3RoZXI model=bW9kZWw -->token quota periods not configured |",
	} {
		if strings.Count(body, expected) != 1 {
			t.Fatalf("The provider table must include each window once and isolate providers for %q.\n%s", expected, body)
		}
	}
	if strings.Count(body, "| `provider` |") != 1 || strings.Count(body, "Captured 2026-10-25 18:00:00 UTC") != 3 {
		t.Fatalf("The provider row must group all available windows with their capture times.\n%s", body)
	}
	t.Log(body[strings.Index(body, "#### Provider details"):])
}

func TestRenderDetailsSeparatesProviderPeriodsFromReviewCosts(t *testing.T) {
	ctx, recorder := review.WithUsageRecorder(t.Context())
	review.RecordModelUsage(ctx, review.ModelUsage{ProviderID: "provider", RequestedModel: "model", Model: "model", Requests: 1, ReportedRequests: 1, InputTokens: 10, TotalTokens: 10, Priced: true, EstimatedCostUSD: 0.02})
	var attempt review.ProviderAttempt
	attempt.Status.ProviderID, attempt.Status.Model, attempt.Count = "provider", "model", 1
	var period review.ProviderPeriodUsage
	period.ProviderID, period.RequestedModel = "provider", "model"
	period.Window.Mode, period.Window.Duration = quota.Rolling, "24h"
	period.Available, period.CostComplete, period.HistoryComplete = true, true, true
	period.TotalTokens, period.EstimatedCostUSD = 1_200_000, 0.24
	var summary review.Summary
	summary.Usage = recorder.Summary()
	summary.Usage.ProviderAttempts = []review.ProviderAttempt{attempt}
	summary.Usage.ProviderColumns = []reviewrules.ProviderDetailColumn{reviewrules.ProviderTokensColumn, reviewrules.ProviderCostColumn, reviewrules.ProviderPeriodUsageColumn}
	monthly := period
	monthly.Window.Mode, monthly.Window.Duration = quota.Cron, ""
	monthly.Window.Schedule, monthly.Window.Timezone = "0 0 1 * *", "UTC"
	monthly.TotalTokens, monthly.EstimatedCostUSD = 2_400_000, 0.48
	summary.Usage.PeriodUsage = []review.ProviderPeriodUsage{period, monthly}
	body := review.RenderDetails(summary)
	for _, expected := range []string{"Total 10", "$0.02000000", "1.2m tokens / ~$0.24", "2.4m tokens / ~$0.48", "| Estimated total cost | $0.02000000 |", "across all models for each provider", "do not represent account charges"} {
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

package review_test

import (
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestRenderDetailsSeparatesPeriodTotalsFromReviewUsage(t *testing.T) {
	ctx, recorder := review.WithUsageRecorder(t.Context())
	var usage review.ModelUsage
	usage.ProviderID, usage.RequestedModel, usage.Model = "provider", "model", "model"
	usage.Requests, usage.ReportedRequests, usage.TotalTokens = 1, 1, 10
	usage.Priced, usage.EstimatedCostUSD = true, 0.02
	review.RecordModelUsage(ctx, usage)
	var attempt review.ProviderAttempt
	attempt.Status.ProviderID, attempt.Status.Model, attempt.Count = "provider", "model", 1
	var daily review.ProviderPeriodUsage
	daily.ProviderID, daily.RequestedModel = "provider", "model"
	daily.Window.Mode, daily.Window.Duration = quota.Rolling, "24h"
	daily.TotalTokens, daily.EstimatedCostUSD = 1_200_000, 0.24
	daily.Available, daily.HistoryComplete, daily.CostComplete = true, true, true
	daily.ReportedRequests, daily.PricedRequests = 2, 2
	daily.AsOfMS = time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC).UnixMilli()
	daily.Bounds.StartMS, daily.Bounds.EndMS = daily.AsOfMS-86_400_000, daily.AsOfMS
	monthly := daily
	monthly.Window.Mode, monthly.Window.Duration = quota.Cron, ""
	monthly.Window.Schedule, monthly.Window.Timezone = "0 0 1 * *", "UTC"
	monthly.TotalTokens, monthly.EstimatedCostUSD = 2_000_000_000, 25.12
	monthly.HistoryComplete, monthly.CostComplete, monthly.PricedRequests = false, false, 1
	monthly.HistoryStartMS = daily.Bounds.StartMS
	weekly := daily
	weekly.Window.Mode, weekly.Window.Duration, weekly.Window.Anchor = quota.Fixed, "168h", "2026-10-01T00:00:00Z"
	weekly.TotalTokens, weekly.PricedRequests, weekly.CostComplete = 1_000, 0, false
	unavailable := daily
	unavailable.Window.Duration, unavailable.Available = "1h", false
	var summary review.Summary
	summary.Usage = recorder.Summary()
	summary.Usage.ProviderAttempts = []review.ProviderAttempt{attempt}
	summary.Usage.ProviderColumns = []reviewrules.ProviderDetailColumn{reviewrules.ProviderTokensColumn, reviewrules.ProviderCostColumn, reviewrules.ProviderPeriodUsageColumn}
	summary.Usage.PeriodUsage = []review.ProviderPeriodUsage{daily, monthly, weekly, unavailable}
	for index := range summary.Usage.PeriodUsage {
		period := &summary.Usage.PeriodUsage[index]
		bounds, err := period.Window.Bounds(time.UnixMilli(period.AsOfMS))
		if err != nil {
			t.Fatal(err)
		}
		period.Bounds = bounds
	}
	body := review.RenderDetails(summary)
	for _, expected := range []string{
		"Total 10", "| Estimated total cost | $0.02000000 |",
		"`rolling` `24h`<br>1.2m tokens / ~$0.24",
		"`cron` `0 0 1 * *` `UTC`<br>2b tokens / ~$25.12",
		"`fixed` `168h`<br>1k tokens / cost unavailable",
		"`rolling` `1h`<br>unavailable",
		"2026-10-24 18:00:00 UTC to 2026-10-25 18:00:00 UTC",
		"Captured 2026-10-25 18:00:00 UTC",
		"Partial history from 2026-10-24 18:00:00 UTC",
		"Prices cover 1 of 2 requests",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("The provider column does not report %q separately from current-review usage.\n%s", expected, body)
		}
	}
	summary.Usage.PeriodUsage = nil
	if unknown := review.RenderDetails(summary); !strings.Contains(unknown, "| unavailable |") {
		t.Fatalf("The period column substituted a value for an unavailable snapshot.\n%s", unknown)
	}
	summary.Usage.ProviderColumns = []reviewrules.ProviderDetailColumn{reviewrules.ProviderCostColumn}
	if disabled := review.RenderDetails(summary); strings.Contains(disabled, "Period usage") {
		t.Fatalf("The renderer included the disabled period column.\n%s", disabled)
	}
	t.Log(body[strings.Index(body, "#### Provider details"):])
}

func TestRenderDetailsShowsRecordedUsageWithoutQuotaWindow(t *testing.T) {
	var period review.ProviderPeriodUsage
	period.ProviderID, period.RequestedModel = "uncapped", "model"
	period.Available, period.CostComplete = true, true
	period.TotalTokens, period.EstimatedCostUSD = 1_200_000, 0.24
	period.ReportedRequests, period.PricedRequests = 2, 2
	period.HistoryStartMS = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).UnixMilli()
	period.AsOfMS = time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC).UnixMilli()
	period.Bounds.StartMS, period.Bounds.EndMS = period.HistoryStartMS, period.AsOfMS
	var attempt review.ProviderAttempt
	attempt.Status.ProviderID, attempt.Status.Model, attempt.Count = "uncapped", "model", 1
	var summary review.Summary
	summary.Usage.ProviderColumns = []reviewrules.ProviderDetailColumn{reviewrules.ProviderIdentityColumn, reviewrules.ProviderPeriodUsageColumn}
	summary.Usage.ProviderAttempts = []review.ProviderAttempt{attempt}
	summary.Usage.PeriodUsage = []review.ProviderPeriodUsage{period}
	body := review.RenderDetails(summary)
	for _, expected := range []string{
		"Recorded usage<br>1.2m tokens / ~$0.24",
		"2026-10-20 00:00:00 UTC to 2026-10-27 00:00:00 UTC",
		"Recorded history begins 2026-10-20 00:00:00 UTC",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("Recorded usage without a quota window is missing %q.\n%s", expected, body)
		}
	}
	if strings.Contains(body, "Period unknown") || strings.Contains(body, "unavailable") {
		t.Fatalf("The renderer discarded known recorded usage.\n%s", body)
	}
}

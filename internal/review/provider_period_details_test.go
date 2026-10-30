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
	monthly.HistoryComplete, monthly.CostComplete = false, false
	monthly.PricedRequests, monthly.ReportedRequests = 2_700, 5_874
	monthly.HistoryStartMS = daily.Bounds.StartMS
	weekly := daily
	weekly.Window.Mode, weekly.Window.Duration, weekly.Window.Anchor = quota.Fixed, "168h", "2026-10-01T00:00:00-04:00"
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
	providerTable, periodDetails := splitProviderPeriodDisplay(t, body)
	for _, expected := range []string{"Total 10", "| Estimated total cost | $0.02000000 |"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("The review does not retain current-review usage %q.\n%s", expected, body)
		}
	}
	for _, expected := range []string{
		"`rolling` `24h`<br>**1.2m tokens**<br>**~$0.24 estimated**",
		"`cron`<br>**2b tokens**<br>**~$25.12 estimated**",
		"`fixed` `168h`<br>**1k tokens**<br>cost unavailable",
		"`rolling` `1h`<br>unavailable",
		"Partial pricing", "Partial history",
	} {
		if !strings.Contains(providerTable, expected) {
			t.Fatalf("The compact provider table does not report %q.\n%s", expected, providerTable)
		}
	}
	for _, expanded := range []string{"2026-10-", "0 0 1 * *", "Requests with known prices", "Partial history from"} {
		if strings.Contains(providerTable, expanded) {
			t.Fatalf("The compact provider table still includes expanded detail %q.\n%s", expanded, providerTable)
		}
	}
	for _, expected := range []string{
		"Usage timestamps use UTC.", "`provider`", "`model`",
		"0 0 1 * *", "UTC", "| Configured anchor | `2026-10-01T00:00:00-04:00` |",
		"| Period start | 2026-10-24 18:00:00 |",
		"| Period end | 2026-10-25 18:00:00 |",
		"| Period start | 2026-10-01 00:00:00 |",
		"| Period end | 2026-11-01 00:00:00 |",
		"| Period start | 2026-10-22 04:00:00 |",
		"| Period end | 2026-10-29 04:00:00 |",
		"| Captured | 2026-10-25 18:00:00 |",
		"| Partial history from | 2026-10-24 18:00:00 |",
		"| Requests with known prices | 2,700 of 5,874 |",
	} {
		if !strings.Contains(periodDetails, expected) {
			t.Fatalf("The full-width period section does not retain %q.\n%s", expected, periodDetails)
		}
	}
	if strings.Count(periodDetails, "| Period start |") < 3 {
		t.Fatalf("The full-width section combines separate available usage periods.\n%s", periodDetails)
	}
	periods := summary.Usage.PeriodUsage
	summary.Usage.PeriodUsage = nil
	if unknown := review.RenderDetails(summary); !strings.Contains(unknown, "| unavailable |") {
		t.Fatalf("The period column substituted a value for an unavailable snapshot.\n%s", unknown)
	}
	summary.Usage.PeriodUsage = periods
	summary.Usage.ProviderColumns = []reviewrules.ProviderDetailColumn{reviewrules.ProviderCostColumn}
	if disabled := review.RenderDetails(summary); strings.Contains(disabled, "Period usage") {
		t.Fatalf("The renderer included disabled period usage.\n%s", disabled)
	}
}

func TestRenderDetailsShowsRecordedUsageWithoutQuotaWindow(t *testing.T) {
	var period review.ProviderPeriodUsage
	period.ProviderID, period.RequestedModel = "uncapped", "model"
	period.Available, period.CostComplete = true, true
	period.TotalTokens, period.EstimatedCostUSD = 1_200_000, 0.24
	period.ReportedRequests, period.PricedRequests = 2, 2
	period.HistoryStartMS = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).UnixMilli()
	period.AsOfMS = time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC).UnixMilli()
	period.Bounds.StartMS, period.Bounds.EndMS = period.HistoryStartMS-1_000, period.AsOfMS
	var attempt review.ProviderAttempt
	attempt.Status.ProviderID, attempt.Status.Model, attempt.Count = "uncapped", "model", 1
	var summary review.Summary
	summary.Usage.ProviderColumns = []reviewrules.ProviderDetailColumn{reviewrules.ProviderIdentityColumn, reviewrules.ProviderPeriodUsageColumn}
	summary.Usage.ProviderAttempts = []review.ProviderAttempt{attempt}
	summary.Usage.PeriodUsage = []review.ProviderPeriodUsage{period}
	body := review.RenderDetails(summary)
	providerTable, periodDetails := splitProviderPeriodDisplay(t, body)
	if !strings.Contains(providerTable, "Recorded usage<br>**1.2m tokens**<br>**~$0.24 estimated**") {
		t.Fatalf("The compact column does not retain recorded usage totals.\n%s", providerTable)
	}
	if strings.Contains(providerTable, "2026-10-") {
		t.Fatalf("The compact column still includes recorded usage timestamps.\n%s", providerTable)
	}
	for _, expected := range []string{
		"`uncapped`", "`model`", "Recorded usage",
		"| Period start | 2026-10-19 23:59:59 |",
		"| Period end | 2026-10-27 00:00:00 |",
		"| Captured | 2026-10-27 00:00:00 |",
		"| Recorded history begins | 2026-10-20 00:00:00 |",
	} {
		if !strings.Contains(periodDetails, expected) {
			t.Fatalf("Recorded usage without a quota window does not retain %q.\n%s", expected, periodDetails)
		}
	}
	if strings.Contains(body, "Period unknown") || strings.Contains(body, "unavailable") {
		t.Fatalf("The renderer discarded known recorded usage.\n%s", body)
	}
	period.EstimatedCostUSD = 0.000041
	summary.Usage.PeriodUsage = []review.ProviderPeriodUsage{period}
	if tinyCost := review.RenderDetails(summary); !strings.Contains(tinyCost, "**~$0.000041 estimated**") {
		t.Fatalf("The compact column rounded a small positive cost to zero.\n%s", tinyCost)
	}
}

func splitProviderPeriodDisplay(t *testing.T, body string) (string, string) {
	t.Helper()
	_, providerDetails, found := strings.Cut(body, "#### Provider details")
	if !found {
		t.Fatalf("The review does not include provider details.\n%s", body)
	}
	providerTable, periodDetails, found := strings.Cut(providerDetails, "#### Period usage details")
	if !found {
		t.Fatalf("The review does not include full-width period usage details.\n%s", body)
	}
	if strings.Count(body, "<summary>Review details</summary>") != 1 || strings.Index(body, "#### Period usage details") > strings.LastIndex(body, "</details>") {
		t.Fatalf("The period section does not use the existing Review details dropdown.\n%s", body)
	}
	return providerTable, periodDetails
}

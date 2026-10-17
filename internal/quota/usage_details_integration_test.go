//go:build integration

package quota_test

import (
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/quota"
)

func TestUsageHistoryRetainsCacheCountersAndReportedServiceTier(t *testing.T) {
	client := quota.NewClient(startBudgetWorker(t), []byte("test-review-budget-secret"), nil)
	now := time.Now().UTC().Truncate(time.Millisecond)
	cached, writes := int64(40), int64(30)
	report := quota.ReportRequest{
		ReportID: "usage-details", ProviderID: "details_provider", Model: "model",
		AdmittedAtMS: now.UnixMilli(),
		Usage: quota.Usage{
			InputTokens: 100, OutputTokens: 10, TotalTokens: 110,
			CachedInputTokens: &cached, CacheWriteTokens: &writes, ServiceTier: "default",
		},
	}
	if err := client.Report(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	if err := client.Report(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	bounds := quota.Bounds{StartMS: now.Add(-time.Minute).UnixMilli(), EndMS: now.Add(time.Minute).UnixMilli(), IncludeStart: true, IncludeEnd: true}
	history, err := client.Query(t.Context(), report.ProviderID, report.Model, bounds)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Events) != 1 {
		t.Fatalf("storage returned %d events for the repeated report", len(history.Events))
	}
	usage := history.Events[0].Usage
	if usage.CachedInputTokens == nil || *usage.CachedInputTokens != cached || usage.CacheWriteTokens == nil || *usage.CacheWriteTokens != writes || usage.ServiceTier != "default" || usage.TotalTokens != 110 {
		t.Fatalf("stored usage lost billing details: %+v", usage)
	}
	writes++
	if err := client.Report(t.Context(), report); err == nil {
		t.Fatal("storage accepted changed cache writes with an existing report identity")
	}
}

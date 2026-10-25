//go:build integration

package quota_test

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestProviderPeriodTotalsUseRealStorageAcrossModelsAndProviders(t *testing.T) {
	endpoint := startBudgetWorker(t)
	storage := quota.NewClient(endpoint, []byte("test-review-budget-secret"), nil)
	now := time.Now().UTC().Truncate(time.Millisecond)
	window := quota.Window{Mode: quota.Fixed, Duration: "24h", Anchor: now.Truncate(time.Hour).Format(time.RFC3339)}
	reportPeriodUsage(t, storage, now, "account_a", config.AutoRouterModel, "routed-v1", 100, 40, 10, 20, 777)
	reportPeriodUsage(t, storage, now, "account_a", "earlier-model", "", 200, 0, 0, 30, 230)
	reportPeriodUsage(t, storage, now, "account_b", config.AutoRouterModel, "routed-v1", 10, 0, 0, 1, 11)
	reportPeriodUsage(t, storage, now.Add(-25*time.Hour), "account_a", config.AutoRouterModel, "routed-v1", 10000, 0, 0, 0, 10000)
	reportPeriodUsage(t, storage, now.Add(time.Hour), "account_a", "future-model", "routed-v1", 10000, 0, 0, 0, 10000)
	loader := periodUsageClient(t, endpoint, window)
	ctx, recorder := review.WithUsageRecorder(t.Context())
	review.RecordModelUsage(ctx, review.ModelUsage{ProviderID: "account_a", RequestedModel: config.AutoRouterModel, TotalTokens: 777, ReportedRequests: 1})
	keys := []review.ProviderModelKey{
		{ProviderID: "account_a", RequestedModel: config.AutoRouterModel},
		{ProviderID: "account_a", RequestedModel: "earlier-model"},
		{ProviderID: "account_b", RequestedModel: config.AutoRouterModel},
	}
	periods, err := loader.LoadProviderPeriodUsage(ctx, keys, now.Add(time.Millisecond))
	if err != nil || len(periods) != 3 {
		t.Fatalf("The provider report must load each display row: %+v, %v", periods, err)
	}
	for _, period := range periods[:2] {
		if !period.Available || period.TotalTokens != 1007 || period.ReportedRequests != 2 || period.PricedRequests != 2 || !period.CostComplete || period.HistoryComplete {
			t.Fatalf("Provider totals must include older requested models and the stored current response exactly once: %+v", period)
		}
		if math.Abs(period.EstimatedCostUSD-0.001458) > 0.000000001 {
			t.Fatalf("The period must use reported-model and cache-aware prices: %+v", period)
		}
	}
	if periods[2].TotalTokens != 11 || !periods[2].Available || recorder.Summary().TotalTokens != 777 {
		t.Fatal("Provider totals must not merge accounts or replace current-review usage.")
	}
	bounds, err := window.Bounds(now.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	bounds.EndMS, bounds.IncludeEnd = now.Add(time.Millisecond).UnixMilli(), true
	operator, err := quota.NewOperatorClient(endpoint, []byte("test-operator-token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	totals, err := operator.SummarizeProvider(t.Context(), "account_a", bounds)
	if err != nil || len(totals.Groups) != 2 {
		t.Fatalf("The authenticated grouped read must preserve requested and reported models: %+v, %v", totals, err)
	}
	encoded, err := json.Marshal(totals)
	if err != nil || strings.Contains(string(encoded), "native-payload-marker") {
		t.Fatal("Grouped reads must not return native SDK payloads.")
	}
	review.RecordProviderUsageDeliveryFailure(ctx, review.ProviderModelKey{ProviderID: "account_a", RequestedModel: "another-model"})
	periods, err = loader.LoadProviderPeriodUsage(ctx, keys, now.Add(time.Millisecond))
	if err != nil || periods[0].Available || periods[1].Available || !periods[2].Available {
		t.Fatalf("An unacknowledged report must make only that provider total unavailable: %+v, %v", periods, err)
	}
	noModelContext, noModelRecorder := review.WithUsageRecorder(t.Context())
	periods, err = loader.LoadProviderPeriodUsage(noModelContext, nil, now.Add(time.Millisecond))
	if err != nil || len(periods) != 2 || !periods[0].Available || noModelRecorder.Summary().Requests != 0 {
		t.Fatalf("A report without model calls must still read configured provider periods: %+v, %v", periods, err)
	}
	if err := storage.Report(t.Context(), quota.ReportRequest{
		ProviderID: "account_a", Model: "unknown-cache", AdmittedAtMS: now.UnixMilli(),
		Usage: quota.Usage{
			InputTokens: 5, OutputTokens: 1, TotalTokens: 6,
			SDKUsage: &quota.SDKUsage{API: "responses", Model: "routed-v1", UsageJSON: `{}`},
		},
	}); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if err := storage.Report(t.Context(), quota.ReportRequest{
		ProviderID: "account_a", Model: "premium-tier", AdmittedAtMS: now.UnixMilli(),
		Usage: quota.Usage{
			InputTokens: 20, OutputTokens: 1, TotalTokens: 21, CachedInputTokens: &zero, CacheWriteTokens: &zero, ServiceTier: "priority",
			SDKUsage: &quota.SDKUsage{API: "responses", Model: "routed-v1", UsageJSON: `{}`},
		},
	}); err != nil {
		t.Fatal(err)
	}
	reportPeriodUsage(t, storage, now, "account_a", "earlier-model", "", 10, 0, 1, 1, 11)
	periods, err = loader.LoadProviderPeriodUsage(noModelContext, nil, now.Add(time.Millisecond))
	if err != nil || !periods[0].Available || periods[0].TotalTokens != 1045 || periods[0].CostComplete || periods[0].PricedRequests != 2 || periods[0].ReportedRequests != 5 || math.Abs(periods[0].EstimatedCostUSD-0.001458) > 0.000000001 {
		t.Fatalf("Missing cache details and unpriced service tiers must retain tokens with an explicitly partial cost: %+v, %v", periods, err)
	}
}

func TestProviderPeriodSummaryRejectsTooManyGroupsWithoutTruncating(t *testing.T) {
	storage := quota.NewClient(startBudgetWorker(t), []byte("test-review-budget-secret"), nil)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for index := range 501 {
		reportPeriodUsage(t, storage, now, "group_limit", "model", fmt.Sprintf("reported-%d", index), 1, 0, 0, 1, 2)
	}
	bounds, err := (quota.Window{Mode: quota.Rolling, Duration: "24h"}).Bounds(now.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	_, err = storage.SummarizeProvider(t.Context(), "group_limit", bounds)
	if err == nil || !strings.Contains(err.Error(), "413") {
		t.Fatalf("An oversized grouped result must fail instead of returning incomplete totals: %v", err)
	}
}

func reportPeriodUsage(t *testing.T, storage *quota.Client, at time.Time, providerID, requested, reported string, input, cached, writes, output, total int64) {
	t.Helper()
	if err := storage.Report(t.Context(), quota.ReportRequest{
		ProviderID: providerID, Model: requested, AdmittedAtMS: at.UnixMilli(),
		Usage: quota.Usage{
			InputTokens: input, OutputTokens: output, TotalTokens: total,
			CachedInputTokens: &cached, CacheWriteTokens: &writes, ServiceTier: "default",
			SDKUsage: &quota.SDKUsage{API: "responses", Model: reported, UsageJSON: `{"native-payload-marker":"` + strings.Repeat("x", 1000) + `"}`},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func periodUsageClient(t *testing.T, endpoint string, window quota.Window) *openai.Client {
	t.Helper()
	budgetURL, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	writes := float64(3)
	return openai.NewClient(config.Config{
		ProviderBudgetURL: budgetURL, GitHubWebhookSecret: []byte("test-review-budget-secret"),
		Providers: []config.ProviderConfig{
			{ID: "account_a", Model: config.AutoRouterModel, UsagePeriod: &window},
			{ID: "account_b", Model: config.AutoRouterModel, UsagePeriod: &window},
		},
		ReviewModelPricing: map[string]config.ModelPricing{
			"routed-v1":     {InputPerMillionTokens: 2, CachedInputPerMillionTokens: 0.2, CacheWritePerMillionTokens: &writes, OutputPerMillionTokens: 8},
			"earlier-model": {InputPerMillionTokens: 4, OutputPerMillionTokens: 12},
		},
	}, nil)
}

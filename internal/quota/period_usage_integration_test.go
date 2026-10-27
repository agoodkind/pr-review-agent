//go:build integration

package quota_test

import (
	"context"
	"math"
	"net/url"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestPeriodDisplayReadsExistingQuotaHistory(t *testing.T) {
	endpoint := startBudgetWorker(t)
	storage := quota.NewClient(endpoint, []byte("test-review-budget-secret"), nil)
	anchor := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	at := anchor.Add(2 * time.Hour)
	fixed := quota.Window{Mode: quota.Fixed, Duration: "24h", Anchor: anchor.Format(time.RFC3339Nano)}
	hour := quota.Window{Mode: quota.Rolling, Duration: "1h"}
	reportDisplayUsage(t, storage, anchor.Add(30*time.Minute), "account", "model", "routed", 20, 0, 0, 2, 22)
	reportDisplayUsage(t, storage, at, "account", "model", "routed", 100, 40, 10, 20, 777)
	reportDisplayUsage(t, storage, at.Add(time.Hour), "account", "model", "routed", 10000, 0, 0, 0, 10000)
	reportDisplayUsage(t, storage, at, "account", "other", "", 10, 0, 0, 1, 11)
	reportDisplayUsage(t, storage, at, "other_account", "model", "routed", 90, 0, 0, 9, 99)
	budgetURL, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	writes := float64(3)
	client := openai.NewClient(config.Config{
		ProviderBudgetURL: budgetURL, GitHubWebhookSecret: []byte("test-review-budget-secret"),
		Providers: []config.ProviderConfig{
			{ID: "account", Model: "model", TokenWindow: fixed, TokenLimits: []quota.Limit{{Limit: 100, Window: hour}, {Limit: 100, Window: fixed}}},
			{ID: "other_account", Model: "model", TokenWindow: fixed},
		},
		ReviewModelPricing: map[string]config.ModelPricing{
			"routed": {InputPerMillionTokens: 2, CachedInputPerMillionTokens: 0.2, CacheWritePerMillionTokens: &writes, OutputPerMillionTokens: 8},
			"other":  {InputPerMillionTokens: 4, OutputPerMillionTokens: 12},
		},
	}, nil)
	ctx, recorder := review.WithUsageRecorder(t.Context())
	review.RecordModelUsage(ctx, review.ModelUsage{ProviderID: "account", RequestedModel: "model", TotalTokens: 777, ReportedRequests: 1})
	keys := []review.ProviderModelKey{{ProviderID: "account", RequestedModel: "model"}, {ProviderID: "account", RequestedModel: "other"}, {ProviderID: "other_account", RequestedModel: "model"}}
	periods, err := client.LoadProviderPeriodUsage(ctx, keys, at)
	if err != nil || len(periods) != 5 {
		t.Fatalf("The display must read each distinct configured window: %+v, %v", periods, err)
	}
	for index, expected := range []int64{799, 777, 11, 11, 99} {
		period := periods[index]
		if !period.Available || !period.HistoryComplete || !period.CostComplete || period.TotalTokens != expected {
			t.Fatalf("Period %d must retain its provider/model bucket and window: %+v", index, period)
		}
	}
	if periods[0].Window != fixed || periods[1].Window != hour || math.Abs(periods[0].EstimatedCostUSD-0.000354) > 1e-12 || recorder.Summary().TotalTokens != 777 {
		t.Fatalf("Stored usage must price reported models without adding the current run twice: %+v", periods)
	}
	if err := storage.Report(t.Context(), quota.ReportRequest{ProviderID: "account", Model: "model", AdmittedAtMS: at.UnixMilli(), Usage: quota.Usage{InputTokens: 1, TotalTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	periods, err = client.LoadProviderPeriodUsage(ctx, keys[:1], at)
	if err != nil || !periods[0].Available || periods[0].CostComplete || periods[0].TotalTokens != 800 || periods[0].PricedRequests != 2 || periods[0].ReportedRequests != 3 {
		t.Fatalf("Missing price evidence must preserve recorded tokens and identify a partial cost: %+v, %v", periods, err)
	}
	client = openai.NewClient(config.Config{
		ProviderBudgetURL: budgetURL, GitHubWebhookSecret: []byte("test-review-budget-secret"),
		Providers: []config.ProviderConfig{{ID: "account", Model: "model", TokenWindow: quota.Window{Mode: quota.Rolling, Duration: "48h"}}},
	}, nil)
	periods, err = client.LoadProviderPeriodUsage(ctx, nil, at)
	if err != nil || !periods[0].Available || periods[0].HistoryComplete || periods[0].TotalTokens != 800 {
		t.Fatalf("A successful read must preserve partial recorded history: %+v, %v", periods, err)
	}
}

func TestPeriodDisplayWithoutStorageRemainsUnavailable(t *testing.T) {
	client := openai.NewClient(config.Config{Providers: []config.ProviderConfig{{ID: "uncapped", Model: "model"}}}, nil)
	periods, err := client.LoadProviderPeriodUsage(t.Context(), nil, time.Now())
	if err != nil || len(periods) != 1 || periods[0].Available || periods[0].Window.Mode != "" {
		t.Fatalf("An uncapped provider without storage must remain unavailable: %+v, %v", periods, err)
	}
}

func TestPeriodDisplayReadsRecordedUsageWithoutAQuotaWindow(t *testing.T) {
	endpoint := startBudgetWorker(t)
	storage := quota.NewClient(endpoint, []byte("test-review-budget-secret"), nil)
	budgetURL, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	client := openai.NewClient(config.Config{
		ProviderBudgetURL: budgetURL, GitHubWebhookSecret: []byte("test-review-budget-secret"),
		Providers:          []config.ProviderConfig{{ID: "uncapped", Model: "model"}},
		ReviewModelPricing: map[string]config.ModelPricing{"routed": {InputPerMillionTokens: 2, OutputPerMillionTokens: 8}},
	}, nil)
	at := time.UnixMilli(1)
	periods, err := client.LoadProviderPeriodUsage(t.Context(), nil, at)
	if err != nil || len(periods) != 1 || !periods[0].Available || periods[0].ReportedRequests != 0 || periods[0].TotalTokens != 0 || !periods[0].CostComplete || periods[0].HistoryStartMS <= 0 {
		t.Fatalf("A successful empty history read must report recorded zero usage with a measured history boundary: %+v, %v", periods, err)
	}
	if periods[0].Bounds.StartMS != at.UnixMilli() || periods[0].Bounds.EndMS != at.UnixMilli() || periods[0].HistoryStartMS <= at.UnixMilli() {
		t.Fatalf("An empty read must retain its measured history boundary without displaying a start after its snapshot: %+v", periods[0])
	}
	at = time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond)
	older := time.UnixMilli(periods[0].HistoryStartMS).Add(-24 * time.Hour)
	reportDisplayUsage(t, storage, older, "uncapped", "model", "routed", 20, 0, 0, 2, 22)
	reportDisplayUsage(t, storage, at, "uncapped", "model", "routed", 100, 0, 0, 20, 777)
	reportDisplayUsage(t, storage, at, "uncapped", "other", "routed", 10000, 0, 0, 0, 10000)
	reportDisplayUsage(t, storage, at, "other_account", "model", "routed", 10000, 0, 0, 0, 10000)
	reportDisplayUsage(t, storage, at.Add(time.Second), "uncapped", "model", "routed", 10000, 0, 0, 0, 10000)
	ctx, recorder := review.WithUsageRecorder(t.Context())
	review.RecordModelUsage(ctx, review.ModelUsage{ProviderID: "uncapped", RequestedModel: "model", TotalTokens: 777, ReportedRequests: 1})
	periods, err = client.LoadProviderPeriodUsage(ctx, nil, at)
	if err != nil || len(periods) != 1 || !periods[0].Available || periods[0].Window.Mode != "" || periods[0].TotalTokens != 799 || periods[0].ReportedRequests != 2 || !periods[0].CostComplete || math.Abs(periods[0].EstimatedCostUSD-0.000416) > 1e-12 {
		t.Fatalf("Recorded usage must preserve the requested-model bucket, reported-model prices, and snapshot cutoff: %+v, %v", periods, err)
	}
	if periods[0].Bounds.StartMS != older.UnixMilli() || periods[0].Bounds.EndMS != at.UnixMilli() || periods[0].AsOfMS != at.UnixMilli() || periods[0].HistoryComplete || recorder.Summary().TotalTokens != 777 {
		t.Fatalf("Recorded dates must include admissions before the history boundary without adding current usage twice: %+v", periods[0])
	}
	if err := storage.Report(t.Context(), quota.ReportRequest{ProviderID: "uncapped", Model: "model", AdmittedAtMS: at.UnixMilli(), Usage: quota.Usage{InputTokens: 1, TotalTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	periods, err = client.LoadProviderPeriodUsage(ctx, nil, at)
	if err != nil || !periods[0].Available || periods[0].TotalTokens != 800 || periods[0].CostComplete || periods[0].PricedRequests != 2 {
		t.Fatalf("Missing pricing evidence must retain known recorded tokens and the priced subtotal: %+v, %v", periods, err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	periods, err = client.LoadProviderPeriodUsage(cancelled, nil, at)
	if err == nil || len(periods) != 1 || periods[0].Available {
		t.Fatalf("A failed recorded-history read must remain unavailable: %+v, %v", periods, err)
	}
}

func reportDisplayUsage(t *testing.T, storage *quota.Client, at time.Time, provider, requested, reported string, input, cached, writes, output, total int64) {
	t.Helper()
	if err := storage.Report(t.Context(), quota.ReportRequest{
		ProviderID: provider, Model: requested, AdmittedAtMS: at.UnixMilli(),
		Usage: quota.Usage{InputTokens: input, CachedInputTokens: &cached, CacheWriteTokens: &writes, OutputTokens: output, TotalTokens: total, SDKUsage: &quota.SDKUsage{API: "responses", Model: reported, UsageJSON: `{}`}},
	}); err != nil {
		t.Fatal(err)
	}
}

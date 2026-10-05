//go:build integration

package quota_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/review"
)

type liveRuntimeProvider struct {
	ID                  string                 `json:"id"`
	BaseURL             string                 `json:"base_url"`
	Model               string                 `json:"model"`
	ReasoningEffort     config.ReasoningEffort `json:"reasoning_effort"`
	API                 config.ProviderAPI     `json:"api_kind"`
	Disabled            bool                   `json:"disabled"`
	MaxOutputTokens     int64                  `json:"max_output_tokens"`
	OmitMaxOutputTokens bool                   `json:"omit_max_output_tokens"`
	OmitTextFormat      bool                   `json:"omit_text_format"`
}

func TestLiveReviewChargesReportedUsageAndDeniesNextRequest(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_QUOTA_TEST") != "1" {
		t.Skip("set PR_AGENT_LIVE_QUOTA_TEST=1 to use a real provider credential")
	}
	provider, minimumImportance := loadLiveProvider(t)
	budgetURL, err := url.Parse(startBudgetWorker(t))
	if err != nil {
		t.Fatal(err)
	}
	provider.TokenLimit = 1
	provider.TokenTypes = []config.TokenType{config.InputTokens, config.OutputTokens}
	provider.TokenWindow = quota.Window{Mode: quota.Rolling, Duration: "24h"}
	client := openai.NewClient(config.Config{
		MinimumImportance:   minimumImportance,
		ProviderBudgetURL:   budgetURL,
		GitHubWebhookSecret: []byte("test-review-budget-secret"), // gitleaks:allow
		Providers:           []config.ProviderConfig{provider},
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctx, usageRecorder := review.WithUsageRecorder(ctx)
	const prompt = "Review this Go function for severe defects using the required schema: func Add(left, right int) int { return left + right }"
	if _, err := client.Review(ctx, prompt); err != nil {
		var apiError *openai.ProviderError
		if errors.As(err, &apiError) {
			t.Fatalf("live provider review failed with HTTP %d", apiError.StatusCode)
		}
		t.Fatal("live provider review failed")
	}
	usage := usageRecorder.Summary()
	if usage.ReportedRequests != 1 || usage.InputTokens+usage.OutputTokens < 1 {
		t.Fatal("live provider did not report actual token usage")
	}
	_, err = client.Review(ctx, prompt)
	var statusError interface{ ProviderStatus() review.ProviderStatus }
	if !errors.As(err, &statusError) {
		t.Fatal("second review did not return a classified quota denial")
	}
	status := statusError.ProviderStatus()
	if status.Cause != review.ProviderAppBudgetDenied || !status.QuotaKnown || status.Limit != 1 || status.Used != usage.InputTokens+usage.OutputTokens {
		t.Fatalf("quota denial = %+v", status)
	}
	bounds, err := provider.TokenWindow.Bounds(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	storage := quota.NewClient(budgetURL.String(), []byte("test-review-budget-secret"), nil)
	reports, err := storage.Query(ctx, provider.ID, provider.Model, bounds)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports.Events) != 1 || reports.Events[0].InputTokens != usage.InputTokens || reports.Events[0].OutputTokens != usage.OutputTokens || reports.Events[0].TotalTokens != usage.TotalTokens {
		t.Fatal("durable accounting differs from actual provider usage")
	}
	t.Logf("live provider=%s model=%s input_tokens=%d output_tokens=%d total_tokens=%d quota_limit=1 next_request=app_budget_denied", provider.ID, provider.Model, usage.InputTokens, usage.OutputTokens, usage.TotalTokens)
}

func loadLiveProvider(t *testing.T) (config.ProviderConfig, int) {
	t.Helper()
	keyFile := os.Getenv("PR_AGENT_LIVE_API_KEY_FILE")
	providerID := os.Getenv("PR_AGENT_LIVE_PROVIDER_ID")
	if keyFile == "" || providerID == "" {
		t.Fatal("PR_AGENT_LIVE_API_KEY_FILE and PR_AGENT_LIVE_PROVIDER_ID are required")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal("cannot read the configured provider credential file")
	}
	apiKey := strings.TrimSpace(string(key))
	if apiKey == "" {
		t.Fatal("the configured provider credential file is empty")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate runtime configuration")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Providers         []liveRuntimeProvider `json:"PROVIDERS"`
		MinimumImportance string                `json:"REVIEW_MIN_IMPORTANCE"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	minimumImportance, err := strconv.Atoi(settings.MinimumImportance)
	if err != nil {
		t.Fatal("runtime minimum importance is not an integer")
	}
	for _, provider := range settings.Providers {
		if provider.ID != providerID {
			continue
		}
		baseURL, err := url.Parse(provider.BaseURL)
		if err != nil || baseURL.Scheme != "https" || baseURL.Hostname() == "" || provider.Disabled || provider.Model == "" {
			t.Fatal("the selected runtime provider must be an enabled HTTPS endpoint")
		}
		api := provider.API
		if api == "" {
			api = config.ResponsesAPI
		}
		return config.ProviderConfig{
			ID: provider.ID, BaseURL: baseURL, Model: provider.Model, APIKey: apiKey, ReasoningEffort: config.ResolveReasoningEffort(provider.ReasoningEffort),
			API: api, MaxOutputTokens: provider.MaxOutputTokens, OmitMaxOutputTokens: provider.OmitMaxOutputTokens, OmitTextFormat: provider.OmitTextFormat,
		}, minimumImportance
	}
	t.Fatal("the selected provider ID is absent from runtime configuration")
	return config.ProviderConfig{}, 0
}

func TestLiveReviewChecksMultipleWindowsAndChargesEachResponseOnce(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_QUOTA_TEST") != "1" {
		t.Skip("set PR_AGENT_LIVE_QUOTA_TEST=1 to use a real provider credential")
	}
	provider, minimumImportance := loadLiveProvider(t)
	budgetURL, err := url.Parse(startBudgetWorker(t))
	if err != nil {
		t.Fatal(err)
	}
	provider.DailyTokenLimit = quota.MaximumInteger
	provider.DailyTokenTypes = []config.TokenType{config.InputTokens, config.OutputTokens}
	provider.TokenLimits = []quota.Limit{
		{Limit: 1, TokenTypes: []quota.TokenType{quota.OutputTokens}, Window: quota.Window{Mode: quota.Rolling, Duration: "1m"}},
		{Limit: quota.MaximumInteger, TokenTypes: []quota.TokenType{quota.InputTokens, quota.OutputTokens}, Window: quota.Window{Mode: quota.Rolling, Duration: "24h"}},
	}
	fallback := provider
	fallback.ID += "_fallback"
	fallback.DailyTokenLimit = 0
	fallback.DailyTokenTypes = nil
	fallback.TokenLimits = nil
	client := openai.NewClient(config.Config{
		MinimumImportance: minimumImportance, ProviderBudgetURL: budgetURL,
		GitHubWebhookSecret: []byte("test-review-budget-secret"), // gitleaks:allow
		Providers:           []config.ProviderConfig{provider, fallback},
	}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	const prompt = "Review changed lines in sum.go. Return concrete findings using the required schema.\n@@ -1 +1 @@\n-func sum(left, right int) int { return left + right }\n+func sum(first, second int) int { return first + second }"
	first := liveReviewUsage(t, ctx, client, prompt)
	storage := quota.NewClient(budgetURL.String(), []byte("test-review-budget-secret"), nil)
	bounds, err := provider.TokenLimits[1].Window.Bounds(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	history, err := storage.Query(ctx, provider.ID, provider.Model, bounds)
	if err != nil || len(history.Events) != 1 || history.Events[0].InputTokens != first.InputTokens || history.Events[0].OutputTokens != first.OutputTokens {
		t.Fatal("the first response did not produce exactly one actual usage report")
	}
	minute, err := quota.Evaluate(provider.TokenLimits[0].Window, time.Now(), 1, provider.TokenLimits[0].TokenTypes, history.Events, history.HistoryStartMS)
	if err != nil || minute.Allowed || minute.Used != first.OutputTokens {
		t.Fatal("the minute limit did not count the first response's output")
	}
	second := liveReviewUsage(t, ctx, client, prompt)
	bounds, err = provider.TokenLimits[1].Window.Bounds(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fallbackHistory, err := storage.Query(ctx, fallback.ID, fallback.Model, bounds)
	if err != nil || len(fallbackHistory.Events) != 1 || fallbackHistory.Events[0].InputTokens != second.InputTokens || fallbackHistory.Events[0].OutputTokens != second.OutputTokens {
		t.Fatal("the denied primary request did not complete through the fallback provider")
	}
	legacy, err := storage.CheckDaily(ctx, provider.ID, provider.Model, provider.DailyTokenLimit)
	if err != nil || legacy.Used == nil || *legacy.Used != first.InputTokens+first.OutputTokens {
		t.Fatal("additional windows duplicated usage or changed the original daily counter")
	}
	expiry := time.UnixMilli(history.Events[0].AdmittedAtMS).Add(time.Minute + time.Millisecond)
	timer := time.NewTimer(time.Until(expiry))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatal("the live quota test expired before the minute window reset")
	case <-timer.C:
	}
	third := liveReviewUsage(t, ctx, client, prompt)
	bounds, err = provider.TokenLimits[1].Window.Bounds(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	history, err = storage.Query(ctx, provider.ID, provider.Model, bounds)
	if err != nil || len(history.Events) != 2 ||
		history.Events[1].InputTokens != third.InputTokens || history.Events[1].OutputTokens != third.OutputTokens {
		t.Fatal("the primary provider did not resume after the minute window expired")
	}
	legacy, err = storage.CheckDaily(ctx, provider.ID, provider.Model, provider.DailyTokenLimit)
	if err != nil || legacy.Used == nil ||
		*legacy.Used != first.InputTokens+first.OutputTokens+third.InputTokens+third.OutputTokens {
		t.Fatal("the resumed response did not update the daily counter exactly once")
	}
	t.Logf("provider=%s model=%s primary_responses=2 fallback_responses=1 recorded_primary_events=2", provider.ID, provider.Model)
}

func liveReviewUsage(t *testing.T, ctx context.Context, client *openai.Client, prompt string) review.UsageSummary {
	t.Helper()
	ctx, recorder := review.WithUsageRecorder(ctx)
	if _, err := client.Review(ctx, prompt); err != nil {
		var apiError *openai.ProviderError
		if errors.As(err, &apiError) {
			t.Fatalf("live provider review failed with status %d and code %s", apiError.StatusCode, apiError.Code)
		}
		t.Fatal("live provider review failed")
	}
	usage := recorder.Summary()
	if usage.ReportedRequests != 1 || usage.InputTokens < 1 || usage.OutputTokens < 1 {
		t.Fatal("the live provider did not report one response's actual input and output usage")
	}
	return usage
}

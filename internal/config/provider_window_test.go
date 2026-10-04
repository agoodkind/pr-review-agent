package config

import (
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/quota"
)

func TestLoadedWindowAndTokenTypesDetermineAdmission(t *testing.T) {
	cfg, err := loadWithOverrides(map[string]string{
		"PROVIDERS":                 `[{"id":"windowed","base_url":"https://provider.example/v1","model":"model","api_key_binding":"KEY","token_limit":50,"token_types":["input"],"token_window":{"mode":"rolling","duration":"15m"}}]`,
		"PROVIDER_PRIORITY":         `["windowed"]`,
		"PROVIDER_WINDOWED_API_KEY": "fixture-windowed-" + strings.Repeat("k", 8),
		"PROVIDER_BUDGET_URL":       "https://budget.example/internal/v1/provider_budget",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC)
	provider := cfg.Providers[0]
	events := []quota.Event{
		{AdmittedAtMS: now.Add(-16 * time.Minute).UnixMilli(), Usage: quota.Usage{InputTokens: 1000, OutputTokens: 10, TotalTokens: 1010}},
		{AdmittedAtMS: now.Add(-time.Minute).UnixMilli(), Usage: quota.Usage{InputTokens: 50, OutputTokens: 900, TotalTokens: 950}},
	}
	snapshot, err := quota.Evaluate(provider.TokenWindow, now, provider.TokenLimit, provider.TokenTypes, events, now.Add(-time.Hour).UnixMilli())
	if err != nil || snapshot.Allowed || snapshot.Used != 50 || snapshot.AvailableAtMS != now.Add(14*time.Minute).UnixMilli() {
		t.Fatalf("loaded quota admission = %+v, error = %v", snapshot, err)
	}
}

func TestLoadRejectsGenericLimitMixedWithZeroDailyAlias(t *testing.T) {
	_, err := loadWithOverrides(map[string]string{
		"PROVIDERS":                 `[{"id":"windowed","base_url":"https://provider.example/v1","model":"model","api_key_binding":"KEY","daily_token_limit":0,"token_limit":50,"token_window":{"mode":"rolling","duration":"15m"}}]`,
		"PROVIDER_PRIORITY":         `["windowed"]`,
		"PROVIDER_WINDOWED_API_KEY": "fixture-windowed-" + strings.Repeat("k", 8),
	})
	if err == nil || !strings.Contains(err.Error(), "cannot mix daily and generic token limits") {
		t.Fatalf("Load mixed quotas = %v", err)
	}
}

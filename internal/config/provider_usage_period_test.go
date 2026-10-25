package config_test

import (
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
)

func TestProviderUsagePeriodDoesNotRequireATokenLimit(t *testing.T) {
	values := map[string]string{
		"PROVIDERS":                  `[{"id":"reporting","base_url":"https://provider.example/v1","model":"model","api_key_binding":"KEY","usage_period":{"mode":"cron","schedule":"0 0 1 * *","timezone":"UTC"}}]`,
		"PROVIDER_PRIORITY":          `["reporting"]`,
		"PROVIDER_REPORTING_API_KEY": "fixture-reporting-" + strings.Repeat("k", 8),
	}
	providers, err := config.LoadProviders(func(name string) (string, bool) { value, found := values[name]; return value, found })
	if err != nil {
		t.Fatal(err)
	}
	provider := providers[0]
	if provider.UsagePeriod == nil || provider.HasTokenLimit() {
		t.Fatal("A reporting period must remain independent of token admission limits.")
	}
	bounds, err := provider.UsagePeriod.Bounds(time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC))
	if err != nil || bounds.StartMS != time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC).UnixMilli() || bounds.EndMS != time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("The loaded period must use the configured calendar resets: %+v, %v", bounds, err)
	}
}

func TestProviderUsagePeriodRejectsInvalidWindows(t *testing.T) {
	values := map[string]string{
		"PROVIDERS":                  `[{"id":"reporting","base_url":"https://provider.example/v1","model":"model","api_key_binding":"KEY","usage_period":{"mode":"cron","schedule":"invalid","timezone":"UTC"}}]`,
		"PROVIDER_PRIORITY":          `["reporting"]`,
		"PROVIDER_REPORTING_API_KEY": "fixture-reporting-" + strings.Repeat("k", 8),
	}
	_, err := config.LoadProviders(func(name string) (string, bool) { value, found := values[name]; return value, found })
	if err == nil || !strings.Contains(err.Error(), "usage_period") {
		t.Fatalf("An invalid reporting window must identify its configuration field: %v", err)
	}
}

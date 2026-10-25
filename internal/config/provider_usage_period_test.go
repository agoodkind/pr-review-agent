package config_test

import (
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/quota"
)

func TestProviderQuotaWindowsSupportUncappedReporting(t *testing.T) {
	for _, fields := range []string{
		`"token_limit":0,"token_window":{"mode":"cron","schedule":"0 0 1 * *","timezone":"UTC"}`,
		`"token_limits":[{"limit":0,"window":{"mode":"rolling","duration":"24h"}}]`,
		`"daily_token_limit":0`,
	} {
		t.Run(fields, func(t *testing.T) {
			values := map[string]string{
				"PROVIDERS":                  `[{"id":"reporting","base_url":"https://provider.example/v1","model":"model","api_key_binding":"KEY",` + fields + `}]`,
				"PROVIDER_PRIORITY":          `["reporting"]`,
				"PROVIDER_REPORTING_API_KEY": "fixture-reporting-" + strings.Repeat("k", 8),
			}
			providers, err := config.LoadProviders(func(name string) (string, bool) { value, found := values[name]; return value, found })
			if err != nil {
				t.Fatal(err)
			}
			provider := providers[0]
			if provider.HasTokenLimit() {
				t.Fatal("Reporting windows must not enable token admission caps.")
			}
			client := openai.NewClient(config.Config{Providers: providers}, nil)
			available, err := client.NextAvailable(t.Context())
			if err != nil || !available.IsZero() {
				t.Fatalf("Uncapped providers must remain available without a budget service: %v, %v", available, err)
			}
			periods, err := client.LoadProviderPeriodUsage(t.Context(), nil, time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC))
			if err != nil || len(periods) != 1 || periods[0].Window.Mode == "" || periods[0].Available {
				t.Fatalf("Reporting must preserve the configured window when storage is unavailable: %+v, %v", periods, err)
			}
			if fields == `"daily_token_limit":0` && (periods[0].Window.Mode != quota.Cron || periods[0].Window.Schedule != "0 0 * * *" || periods[0].Window.Timezone != "UTC") {
				t.Fatalf("The legacy daily setting must retain its UTC calendar day: %+v", periods[0])
			}
		})
	}
}

func TestProviderQuotaWindowsRejectInvalidReportingWindows(t *testing.T) {
	values := map[string]string{
		"PROVIDERS":                  `[{"id":"reporting","base_url":"https://provider.example/v1","model":"model","api_key_binding":"KEY","token_limits":[{"limit":0,"window":{"mode":"cron","schedule":"invalid","timezone":"UTC"}}]}]`,
		"PROVIDER_PRIORITY":          `["reporting"]`,
		"PROVIDER_REPORTING_API_KEY": "fixture-reporting-" + strings.Repeat("k", 8),
	}
	_, err := config.LoadProviders(func(name string) (string, bool) { value, found := values[name]; return value, found })
	if err == nil || !strings.Contains(err.Error(), "token_limits") {
		t.Fatalf("An invalid reporting window must identify its quota field: %v", err)
	}
}

func TestManualDailyQuotaUsesUTCDayForReporting(t *testing.T) {
	client := openai.NewClient(config.Config{Providers: []config.ProviderConfig{{ID: "daily", Model: "model", DailyTokenLimit: 10}}}, nil)
	at := time.Date(2026, time.October, 25, 12, 0, 0, 0, time.UTC)
	periods, err := client.LoadProviderPeriodUsage(t.Context(), nil, at)
	if err != nil || len(periods) != 1 || periods[0].Bounds.StartMS != at.Add(-12*time.Hour).UnixMilli() || periods[0].Bounds.EndMS != at.Add(12*time.Hour).UnixMilli() {
		t.Fatalf("A manually configured daily cap must report its UTC day: %+v, %v", periods, err)
	}
}

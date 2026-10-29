//go:build integration

package quota_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestUncappedProductionRuntimeRetainsAccounting(t *testing.T) {
	endpoint, transport := accountingWorkerTLS(t, 0)
	cfg := loadUncappedRuntime(t, endpoint)
	for _, provider := range cfg.Providers {
		if provider.HasTokenLimit() {
			t.Fatal("The runtime regression requires providers without app token limits.")
		}
	}
	provider := cfg.Providers[0]
	storage := quota.NewClient(endpoint, cfg.GitHubWebhookSecret, transport.Client())
	at := time.Now().UTC().Truncate(time.Millisecond)
	reportDisplayUsage(t, storage, at, provider.ID, provider.Model, provider.Model, 20, 0, 0, 2, 22)
	client := openai.NewClient(cfg, transport.Client())
	periods, err := client.LoadProviderPeriodUsage(t.Context(), nil, at.Add(time.Second))
	if err != nil || len(periods) != len(cfg.Providers) {
		t.Fatalf("Read configured provider histories: %v", err)
	}
	if !periods[0].Available || periods[0].TotalTokens != 22 || periods[0].ReportedRequests != 1 {
		t.Fatalf("The uncapped provider must retain its stored usage: %+v", periods[0])
	}
	for _, period := range periods {
		if !period.Available || period.Window.Mode != "" {
			t.Fatalf("Accounting must remain available without adding a quota window: %+v", period)
		}
	}
	var summary review.Summary
	summary.Usage.ProviderColumns = cfg.ReviewPolicy.ProviderDetails()
	summary.Usage.ProviderAttempts = []review.ProviderAttempt{{Status: review.ProviderStatus{ProviderID: provider.ID, Model: provider.Model}, Count: 1}}
	summary.Usage.PeriodUsage = periods
	body := review.RenderDetails(summary)
	if !strings.Contains(body, "Recorded usage<br>22 tokens /") || strings.Contains(body, "Recorded usage<br>unavailable") {
		t.Fatalf("The review must display the stored uncapped usage: %s", body)
	}
}

func TestAccountingHistoryQueriesHaveIndependentTimeouts(t *testing.T) {
	endpoint, transport := accountingWorkerTLS(t, time.Second)
	cfg := loadUncappedRuntime(t, endpoint)
	provider := cfg.Providers[0]
	var keys []review.ProviderModelKey
	for index := range 6 {
		keys = append(keys, review.ProviderModelKey{ProviderID: provider.ID, RequestedModel: fmt.Sprintf("history-%d", index)})
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	storage := quota.NewClient(endpoint, cfg.GitHubWebhookSecret, transport.Client())
	reportDisplayUsage(t, storage, at, provider.ID, keys[0].RequestedModel, provider.Model, 20, 0, 0, 2, 22)
	client := openai.NewClient(cfg, transport.Client())
	periods, err := client.LoadProviderPeriodUsage(t.Context(), keys, at.Add(time.Minute))
	if err != nil || len(periods) != len(keys) {
		t.Fatalf("Each delayed history query must have its own timeout: %v", err)
	}
	for _, period := range periods {
		if !period.Available {
			t.Fatalf("An earlier query consumed the timeout of a later history: %+v", period)
		}
	}
	if periods[0].TotalTokens != 22 {
		t.Fatalf("The delayed history must retain stored usage: %+v", periods[0])
	}
}

func accountingWorkerTLS(t *testing.T, latency time.Duration) (string, *httptest.Server) {
	t.Helper()
	target, err := url.Parse(startBudgetWorker(t))
	if err != nil {
		t.Fatal(err)
	}
	path := target.Path
	target.Path = ""
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(latency)
		proxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	return server.URL + path, server
}

func loadUncappedRuntime(t *testing.T, endpoint string) config.Config {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var runtime map[string]json.RawMessage
	if err := json.Unmarshal(data, &runtime); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"PROVIDER_BUDGET_URL": endpoint,
		"REVIEW_RULES_FILE":   filepath.Join(root, "config", "review-rules.json"),
		"REVIEW_PROMPTS_FILE": filepath.Join(root, "config", "review-prompts.json"),
	} {
		runtime[name], err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	credentials := map[string]string{
		"GITHUB_PRIVATE_KEY":                     string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})),
		"GITHUB_WEBHOOK_SECRET":                  "test-review-budget-secret",
		"PROVIDER_OPENAI_GOODKIND_IO_API_KEY":    "test-key",
		"PROVIDER_OPENAI_GOODKINDALEX_API_KEY":   "test-key",
		"PROVIDER_GEMINI_38_API_KEY":             "test-key",
		"PROVIDER_GEMINI_API_KEY":                "test-key",
		"PROVIDER_OPENROUTER_API_KEY":            "test-key",
		"PROVIDER_CLYDE_API_KEY":                 "test-key",
		"PROVIDER_CLYDE_CF_ACCESS_CLIENT_ID":     "test-id",
		"PROVIDER_CLYDE_CF_ACCESS_CLIENT_SECRET": "test-secret",
	}
	data, err = json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadRuntime(data, func(name string) (string, bool) {
		value, found := credentials[name]
		return value, found
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

//go:build integration

package quota_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/quota"
)

func TestOperatorCLIReadsRealQuotaCounters(t *testing.T) {
	url := startBudgetWorker(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	signed := quota.NewClient(url, []byte("test-review-budget-secret"), nil)
	legacyTokens := int64(300)
	if err := signed.Report(t.Context(), quota.ReportRequest{
		ProviderID: "operator_test", Model: "test_model", AdmittedAtMS: now.UnixMilli(),
		Usage:     quota.Usage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300},
		LegacyDay: now.Format("2006-01-02"), LegacyTokens: &legacyTokens,
	}); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "operator-token")
	if err := os.WriteFile(tokenFile, []byte("test-operator-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration := struct {
		URL       string `json:"PROVIDER_BUDGET_URL"`
		Providers []struct {
			ID          string        `json:"id"`
			Model       string        `json:"model"`
			Limit       int64         `json:"daily_token_limit"`
			TokenLimits []quota.Limit `json:"token_limits"`
		} `json:"PROVIDERS"`
	}{URL: url}
	configuration.Providers = append(configuration.Providers, struct {
		ID          string        `json:"id"`
		Model       string        `json:"model"`
		Limit       int64         `json:"daily_token_limit"`
		TokenLimits []quota.Limit `json:"token_limits"`
	}{ID: "operator_test", Model: "test_model", Limit: 1000, TokenLimits: []quota.Limit{
		{Limit: 100, TokenTypes: []quota.TokenType{quota.OutputTokens}, Window: quota.Window{Mode: quota.Rolling, Duration: "1m"}},
		{Limit: 1000, TokenTypes: []quota.TokenType{quota.InputTokens}, Window: quota.Window{Mode: quota.Rolling, Duration: "24h"}},
	}})
	data, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(directory, "runtime.json")
	if err := os.WriteFile(runtimePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(directory, "capture")
	command := exec.CommandContext(t.Context(), "go", "run", "../../cmd/pragent-ops", "counters", "--runtime", runtimePath, "--operator-token-file", tokenFile, "--output-dir", outputPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("operator CLI failed: %v: %s", err, output)
	}
	data, err = os.ReadFile(filepath.Join(outputPath, "counters.json"))
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Counters []struct {
			Legacy      quota.DailySnapshot `json:"legacy_daily_snapshot"`
			TokenLimits []struct {
				Snapshot quota.Snapshot      `json:"snapshot"`
				History  quota.QueryResponse `json:"history"`
			} `json:"token_limits"`
		} `json:"counters"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Counters) != 1 || capture.Counters[0].Legacy.Used == nil || *capture.Counters[0].Legacy.Used != 300 || capture.Counters[0].Legacy.Remaining == nil || *capture.Counters[0].Legacy.Remaining != 700 {
		t.Fatalf("operator CLI returned incorrect usage: %+v", capture)
	}
	limits := capture.Counters[0].TokenLimits
	if len(limits) != 2 || limits[0].Snapshot.Allowed || limits[0].Snapshot.Used != 100 ||
		limits[0].Snapshot.Remaining != 0 || limits[1].Snapshot.Used != 200 || limits[1].Snapshot.Remaining != 800 ||
		len(limits[0].History.Events) != 1 || len(limits[1].History.Events) != 1 {
		t.Fatal("operator CLI did not report each configured token window using the same stored usage")
	}
	operator, err := quota.NewOperatorClient(url, []byte("test-operator-token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	bounds, err := (quota.Window{Mode: quota.Rolling, Duration: "24h"}).Bounds(now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	history, err := operator.Query(t.Context(), "operator_test", "test_model", bounds)
	if err != nil || len(history.Events) != 1 || history.Events[0].TotalTokens != 300 {
		t.Fatalf("operator history query failed: %+v, %v", history, err)
	}
	if err := operator.Report(t.Context(), quota.ReportRequest{ProviderID: "operator_test", Model: "test_model", AdmittedAtMS: now.UnixMilli(), Usage: quota.Usage{TotalTokens: 1}}); err == nil {
		t.Fatal("operator token permitted a usage report")
	}
}

func TestClientUsesRealDurableStorageForSelectedUsageAndPagination(t *testing.T) {
	url := startBudgetWorker(t)
	client := quota.NewClient(url, []byte("test-review-budget-secret"), nil)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	window := quota.Window{Mode: quota.Rolling, Duration: "24h"}
	bounds, err := window.Bounds(now)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := client.Query(ctx, "integration_provider", "configured_model", bounds)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Events) != 0 || initial.HistoryStartMS == 0 {
		t.Fatalf("initial history = %+v", initial)
	}
	for index := range 501 {
		if err := client.Report(ctx, quota.ReportRequest{
			ProviderID: "integration_provider", Model: "configured_model",
			AdmittedAtMS: now.Add(-time.Hour).UnixMilli(), Usage: quota.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3},
		}); err != nil {
			t.Fatalf("report %d: %v", index, err)
		}
	}
	legacy, err := client.CheckDaily(ctx, "legacy_integration", "configured_model", 100)
	if err != nil {
		t.Fatal(err)
	}
	legacyTokens := int64(100)
	if err := client.Report(ctx, quota.ReportRequest{
		ProviderID: "legacy_integration", Model: "configured_model", AdmittedAtMS: now.UnixMilli(),
		Usage: quota.Usage{InputTokens: 70, OutputTokens: 30, TotalTokens: 100}, LegacyDay: legacy.Day, LegacyTokens: &legacyTokens,
	}); err != nil {
		t.Fatal(err)
	}
	legacy, err = client.CheckDaily(ctx, "legacy_integration", "configured_model", 100)
	if err != nil || legacy.Allowed || legacy.Used == nil || *legacy.Used != 100 {
		t.Fatalf("legacy snapshot = %+v, error = %v", legacy, err)
	}
	page, err := client.Query(ctx, "integration_provider", "configured_model", bounds)
	if err != nil || len(page.Events) != 501 {
		t.Fatalf("query count = %d, error = %v", len(page.Events), err)
	}
	snapshot, err := quota.Evaluate(window, now, 1000, []quota.TokenType{quota.InputTokens}, page.Events, page.HistoryStartMS)
	if err != nil || snapshot.Allowed || snapshot.Used != 1002 || snapshot.HistoryComplete {
		t.Fatalf("rolling snapshot = %+v, error = %v", snapshot, err)
	}
	other, err := client.Query(ctx, "integration_provider", "other_model", bounds)
	if err != nil || len(other.Events) != 0 {
		t.Fatalf("other model = %+v, error = %v", other, err)
	}
}

func startBudgetWorker(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate integration test")
	}
	directory := filepath.Join(filepath.Dir(source), "..", "..", "deploy", "cloudflare")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.CreateTemp(t.TempDir(), "worker-*.log")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("test-operator-token"))
	command := exec.Command(filepath.Join(directory, "node_modules", ".bin", "wrangler"), "dev", "--config", "test/budget.wrangler.jsonc", "--local", "--port", strconv.Itoa(port), "--persist-to", t.TempDir(), "--var", "OPERATOR_TOKEN_SHA256:"+hex.EncodeToString(digest[:]))
	command.Dir = directory
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := command.Process.Signal(os.Interrupt); err != nil {
			t.Log(err)
		}
		if err := command.Wait(); err != nil {
			t.Log(err)
		}
		if err := logFile.Close(); err != nil {
			t.Log(err)
		}
	})
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/internal/v1/provider_budget"
	probeClient := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		response, err := probeClient.Get(url)
		if err == nil {
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			return url
		}
		time.Sleep(100 * time.Millisecond)
	}
	output, err := os.ReadFile(logFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("worker did not start: %s", output)
	return ""
}

//go:build integration

package quota_test

import (
	"context"
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
	command := exec.Command(filepath.Join(directory, "node_modules", ".bin", "wrangler"), "dev", "--config", "test/budget.wrangler.jsonc", "--local", "--port", strconv.Itoa(port), "--persist-to", t.TempDir())
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

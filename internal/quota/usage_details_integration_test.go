//go:build integration

package quota_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
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
			SDKUsage: &quota.SDKUsage{
				API: "responses", Model: "reported_model", ResponseID: "response_identity",
				UsageJSON: `{ "future_count": 9007199254740993, "future": {"explicit_null": null, "nested": [true, {"preserved": "` + strings.Repeat("x", 2048) + `"}] } }`,
			},
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
	if usage.SDKUsage == nil || *usage.SDKUsage != *report.SDKUsage {
		t.Fatal("stored usage changed the native SDK envelope or JSON text")
	}
	writes++
	if err := client.Report(t.Context(), report); err == nil {
		t.Fatal("storage accepted changed cache writes with an existing report identity")
	}
	writes--
	changed := *report.SDKUsage
	changed.UsageJSON = `{"future_count":9007199254740994}`
	report.SDKUsage = &changed
	if err := client.Report(t.Context(), report); err == nil {
		t.Fatal("storage accepted changed native SDK usage with an existing report identity")
	}
	history, err = client.Query(t.Context(), report.ProviderID, report.Model, bounds)
	if err != nil || len(history.Events) != 1 || history.Events[0].TotalTokens != 110 || history.Events[0].SDKUsage == nil || *history.Events[0].SDKUsage != *usage.SDKUsage {
		t.Fatal("a conflicting native SDK report changed existing usage")
	}
}

func TestNativeUsageRequiresStorageAcknowledgmentWithoutRetrying(t *testing.T) {
	endpoint := startBudgetWorker(t)
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	path := target.Path
	target.Path = ""
	var requests atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(request *http.Request) {
		director(request)
		request.Header.Set("Accept-Encoding", "identity")
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		requests.Add(1)
		var acknowledgment map[string]json.RawMessage
		if err := json.NewDecoder(response.Body).Decode(&acknowledgment); err != nil {
			return err
		}
		if err := response.Body.Close(); err != nil {
			return err
		}
		delete(acknowledgment, "sdk_usage_stored")
		data, err := json.Marshal(acknowledgment)
		if err != nil {
			return err
		}
		response.Body = io.NopCloser(bytes.NewReader(data))
		response.ContentLength = int64(len(data))
		response.Header.Set("Content-Length", strconv.Itoa(len(data)))
		return nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	client := quota.NewClient(server.URL+path, []byte("test-review-budget-secret"), nil)
	now := time.Now().UTC().Truncate(time.Millisecond)
	err = client.Report(t.Context(), quota.ReportRequest{
		ReportID: "unacknowledged-native", ProviderID: "native_ack", Model: "model", AdmittedAtMS: now.UnixMilli(),
		Usage: quota.Usage{
			InputTokens: 3, OutputTokens: 2, TotalTokens: 5,
			SDKUsage: &quota.SDKUsage{API: "responses", UsageJSON: `{"unknown":null}`},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "did not acknowledge native SDK usage storage") || requests.Load() != 1 {
		t.Fatalf("native storage acknowledgment failure = %v, attempts=%d", err, requests.Load())
	}
	storage := quota.NewClient(endpoint, []byte("test-review-budget-secret"), nil)
	history, err := storage.Query(t.Context(), "native_ack", "model", quota.Bounds{StartMS: now.Add(-time.Minute).UnixMilli(), EndMS: now.Add(time.Minute).UnixMilli(), IncludeStart: true, IncludeEnd: true})
	if err != nil || len(history.Events) != 1 || history.Events[0].SDKUsage == nil || history.Events[0].TotalTokens != 5 {
		t.Fatal("the missing acknowledgment changed or duplicated committed native usage")
	}
}

func TestUsageHistoryPaginatesCompleteSDKEnvelopesWithoutChangingQuotaTotals(t *testing.T) {
	client := quota.NewClient(startBudgetWorker(t), []byte("test-review-budget-secret"), nil)
	now := time.Now().UTC().Truncate(time.Millisecond)
	const requests = 20
	native := &quota.SDKUsage{
		API: "generate_content", Model: "reported_model", ResponseID: "response_identity",
		UsageJSON: `{"future_metadata":"` + strings.Repeat("界", 3000) + `","future_count":123456789012345678901234567890}`,
	}
	for index := range requests {
		if err := client.Report(t.Context(), quota.ReportRequest{
			ProviderID: "sdk_pages", Model: "configured_model", AdmittedAtMS: now.Add(-time.Duration(index) * time.Millisecond).UnixMilli(),
			Usage: quota.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3, SDKUsage: native},
		}); err != nil {
			t.Fatal(err)
		}
	}
	bounds := quota.Bounds{StartMS: now.Add(-time.Minute).UnixMilli(), EndMS: now.Add(time.Minute).UnixMilli(), IncludeStart: true, IncludeEnd: true}
	history, err := client.Query(t.Context(), "sdk_pages", "configured_model", bounds)
	if err != nil || len(history.Events) != requests {
		t.Fatalf("byte-bounded usage pages returned %d events: %v", len(history.Events), err)
	}
	for _, event := range history.Events {
		if event.SDKUsage == nil || *event.SDKUsage != *native {
			t.Fatal("byte-bounded history changed the native usage JSON")
		}
	}
	snapshot, err := quota.Evaluate(quota.Window{Mode: quota.Rolling, Duration: "1h"}, now.Add(time.Second), 1000, []quota.TokenType{quota.InputTokens, quota.OutputTokens}, history.Events, history.HistoryStartMS)
	if err != nil || snapshot.Used != requests*3 {
		t.Fatalf("native SDK metadata changed canonical accounting: %+v, %v", snapshot, err)
	}
}

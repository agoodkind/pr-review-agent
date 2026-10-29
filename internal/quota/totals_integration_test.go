//go:build integration

package quota_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/quota"
)

func TestUsageTotalsPreservePricingInputsAndStoredSDKEnvelopes(t *testing.T) {
	endpoint := startBudgetWorker(t)
	storage := quota.NewClient(endpoint, []byte("test-review-budget-secret"), nil)
	at := time.Now().UTC().Truncate(time.Millisecond)
	native := &quota.SDKUsage{API: "responses", Model: "routed", ResponseID: "native-response", UsageJSON: `{"future_metadata":"` + strings.Repeat("界", 1000) + `","future_count":123456789012345678901234567890}`}
	cases := []quota.Usage{
		{InputTokens: 100, OutputTokens: 20, TotalTokens: 125, CachedInputTokens: totalsCounter(40), CacheWriteTokens: totalsCounter(10), ServiceTier: "default", SDKUsage: native},
		{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, CachedInputTokens: totalsCounter(0), CacheWriteTokens: totalsCounter(0), ServiceTier: "default", SDKUsage: &quota.SDKUsage{API: "responses", Model: "routed", UsageJSON: `{}`}},
		{InputTokens: 50, OutputTokens: 4, TotalTokens: 55, ServiceTier: "priority", SDKUsage: &quota.SDKUsage{API: "responses", Model: "routed", UsageJSON: `{}`}},
		{InputTokens: 7, OutputTokens: 1, TotalTokens: 8, CachedInputTokens: totalsCounter(0), ServiceTier: "default", SDKUsage: &quota.SDKUsage{API: "responses", Model: "routed", UsageJSON: `{}`}},
		{InputTokens: 3, OutputTokens: 2, TotalTokens: 5},
		{InputTokens: 4, OutputTokens: 1, TotalTokens: 5, CachedInputTokens: totalsCounter(0), CacheWriteTokens: totalsCounter(0), SDKUsage: &quota.SDKUsage{API: "responses", UsageJSON: `{}`}},
		{InputTokens: 1, OutputTokens: 1, TotalTokens: 2, CacheWriteTokens: totalsCounter(0), ServiceTier: "default", SDKUsage: &quota.SDKUsage{API: "responses", Model: "routed", UsageJSON: `{}`}},
		{InputTokens: 1, TotalTokens: 1, CachedInputTokens: totalsCounter(0), CacheWriteTokens: totalsCounter(0), ServiceTier: "default", SDKUsage: &quota.SDKUsage{API: "responses", Model: "routed", UsageJSON: `{}`}},
		{OutputTokens: 6, TotalTokens: 6},
	}
	for index, usage := range cases {
		if err := storage.Report(t.Context(), quota.ReportRequest{ProviderID: "totals", Model: "configured", AdmittedAtMS: at.Add(time.Duration(index) * time.Millisecond).UnixMilli(), Usage: usage}); err != nil {
			t.Fatal(err)
		}
	}
	for _, excluded := range []quota.ReportRequest{
		{ProviderID: "another", Model: "configured", AdmittedAtMS: at.UnixMilli(), Usage: quota.Usage{TotalTokens: 1000}},
		{ProviderID: "totals", Model: "another", AdmittedAtMS: at.UnixMilli(), Usage: quota.Usage{TotalTokens: 1000}},
		{ProviderID: "totals", Model: "configured", AdmittedAtMS: at.Add(time.Hour).UnixMilli(), Usage: quota.Usage{TotalTokens: 1000}},
	} {
		if err := storage.Report(t.Context(), excluded); err != nil {
			t.Fatal(err)
		}
	}
	bounds := quota.Bounds{StartMS: at.UnixMilli(), EndMS: at.Add(time.Second).UnixMilli(), IncludeStart: true, IncludeEnd: true}
	before, err := storage.Query(t.Context(), "totals", "configured", bounds)
	if err != nil || len(before.Events) != len(cases) {
		t.Fatalf("Read original SDK history: count=%d, error=%v", len(before.Events), err)
	}
	operator, err := quota.NewOperatorClient(endpoint, []byte("test-operator-token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	totals, err := operator.QueryTotals(t.Context(), "totals", "configured", bounds)
	if err != nil {
		t.Fatal(err)
	}
	want := []quota.UsageTotalsGroup{
		{ReportedModel: "configured", Requests: 1, FirstAdmittedAtMS: at.Add(4 * time.Millisecond).UnixMilli(), Usage: cases[4]},
		{ReportedModel: "configured", Requests: 1, FirstAdmittedAtMS: at.Add(8 * time.Millisecond).UnixMilli(), Usage: cases[8]},
		{ReportedModel: "configured", Requests: 1, FirstAdmittedAtMS: at.Add(5 * time.Millisecond).UnixMilli(), Usage: quota.Usage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5, CachedInputTokens: totalsCounter(0), CacheWriteTokens: totalsCounter(0)}},
		{ReportedModel: "routed", Requests: 1, FirstAdmittedAtMS: at.Add(6 * time.Millisecond).UnixMilli(), Usage: quota.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2, CacheWriteTokens: totalsCounter(0), ServiceTier: "default"}},
		{ReportedModel: "routed", Requests: 1, FirstAdmittedAtMS: at.Add(3 * time.Millisecond).UnixMilli(), Usage: quota.Usage{InputTokens: 7, OutputTokens: 1, TotalTokens: 8, CachedInputTokens: totalsCounter(0), ServiceTier: "default"}},
		{ReportedModel: "routed", Requests: 2, FirstAdmittedAtMS: at.Add(time.Millisecond).UnixMilli(), Usage: quota.Usage{InputTokens: 11, OutputTokens: 2, TotalTokens: 13, CachedInputTokens: totalsCounter(0), CacheWriteTokens: totalsCounter(0), ServiceTier: "default"}},
		{ReportedModel: "routed", Requests: 1, FirstAdmittedAtMS: at.UnixMilli(), Usage: quota.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 125, CachedInputTokens: totalsCounter(40), CacheWriteTokens: totalsCounter(10), ServiceTier: "default"}},
		{ReportedModel: "routed", Requests: 1, FirstAdmittedAtMS: at.Add(2 * time.Millisecond).UnixMilli(), Usage: quota.Usage{InputTokens: 50, OutputTokens: 4, TotalTokens: 55, ServiceTier: "priority"}},
	}
	if !reflect.DeepEqual(totals.Groups, want) || totals.SnapshotSequence != before.SnapshotSequence || totals.HistoryStartMS != before.HistoryStartMS {
		t.Fatalf("Grouped totals changed pricing inputs or history identity: %+v", totals)
	}
	totalsPrice(t, totals.Groups, nil, 0.000102, 4)
	writePrice := float64(3)
	totalsPrice(t, totals.Groups, &writePrice, 0.0004, 5)
	after, err := storage.Query(t.Context(), "totals", "configured", bounds)
	if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(after.Events[0].SDKUsage, native) {
		t.Fatal("Reading totals changed stored events or native SDK envelopes.")
	}
	for _, flags := range []struct {
		start, end bool
		count      int64
	}{{false, false, 6}, {false, true, 7}, {true, false, 7}, {true, true, 8}} {
		selected := quota.Bounds{StartMS: at.UnixMilli(), EndMS: at.Add(7 * time.Millisecond).UnixMilli(), IncludeStart: flags.start, IncludeEnd: flags.end}
		result, err := storage.QueryTotals(t.Context(), "totals", "configured", selected)
		if err != nil {
			t.Fatal(err)
		}
		var count int64
		for _, group := range result.Groups {
			count += group.Requests
		}
		if count != flags.count {
			t.Fatalf("Inclusive boundaries counted %d requests, want %d", count, flags.count)
		}
	}
	t.Run("overflow", func(t *testing.T) {
		for range 2 {
			if err := storage.Report(t.Context(), quota.ReportRequest{ProviderID: "overflow_totals", Model: "configured", AdmittedAtMS: at.UnixMilli(), Usage: quota.Usage{TotalTokens: quota.MaximumInteger}}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := storage.QueryTotals(t.Context(), "overflow_totals", "configured", bounds); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
			t.Fatalf("Unsafe grouped totals must fail explicitly: %v", err)
		}
	})
}

func TestUsageTotalsPaginationPreservesSequenceSnapshotAndByteBound(t *testing.T) {
	endpoint := startBudgetWorker(t)
	storage := quota.NewClient(endpoint, []byte("test-review-budget-secret"), nil)
	at := time.Now().UTC().Truncate(time.Millisecond)
	const requests = 501
	modelSuffix := strings.Repeat("界", 100)
	nativeJSON := `{"future_usage":"` + strings.Repeat("retained", 100) + `"}`
	for index := range requests {
		usage := quota.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3, SDKUsage: &quota.SDKUsage{API: "responses", Model: fmt.Sprintf("%04d-%s", index, modelSuffix), UsageJSON: nativeJSON}}
		if err := storage.Report(t.Context(), quota.ReportRequest{ProviderID: "paged_totals", Model: "configured", AdmittedAtMS: at.UnixMilli(), Usage: usage}); err != nil {
			t.Fatal(err)
		}
	}
	maximumBytes := totalsConfiguredPageBytes(t)
	var pages atomic.Int64
	var initialSnapshot atomic.Int64
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	path := target.Path
	target.Path = ""
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(request *http.Request) {
		director(request)
		request.Header.Set("Accept-Encoding", "identity")
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		if err := response.Body.Close(); err != nil {
			return err
		}
		response.Body = io.NopCloser(bytes.NewReader(data))
		var page struct {
			quota.TotalsResponse
			NextGroup int64 `json:"next_group"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return err
		}
		if len(data) > maximumBytes || len(page.Groups) > 500 {
			t.Errorf("The real Worker exceeded its configured page bound: bytes=%d groups=%d", len(data), len(page.Groups))
		}
		if pages.Add(1) != 1 {
			return nil
		}
		initialSnapshot.Store(page.SnapshotSequence)
		if page.NextGroup <= 0 || len(page.Groups) >= 500 {
			t.Errorf("The first totals page did not apply its byte bound: %+v", page)
		}
		for _, reported := range []string{"0000-" + modelSuffix, "0000-before"} {
			if err := storage.Report(t.Context(), quota.ReportRequest{ProviderID: "paged_totals", Model: "configured", AdmittedAtMS: at.UnixMilli(), Usage: quota.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3, SDKUsage: &quota.SDKUsage{API: "responses", Model: reported, UsageJSON: nativeJSON}}}); err != nil {
				return err
			}
		}
		return nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	client := quota.NewClient(server.URL+path, []byte("test-review-budget-secret"), server.Client())
	bounds := quota.Bounds{StartMS: at.UnixMilli(), EndMS: at.Add(time.Second).UnixMilli(), IncludeStart: true, IncludeEnd: true}
	result, err := client.QueryTotals(t.Context(), "paged_totals", "configured", bounds)
	if err != nil || len(result.Groups) != requests || pages.Load() < 2 || result.SnapshotSequence != initialSnapshot.Load() {
		t.Fatalf("Paginated totals lost their initial snapshot: groups=%d pages=%d error=%v", len(result.Groups), pages.Load(), err)
	}
	for index, group := range result.Groups {
		if group.ReportedModel != fmt.Sprintf("%04d-%s", index, modelSuffix) || group.Requests != 1 || group.InputTokens != 2 || group.OutputTokens != 1 || group.TotalTokens != 3 || group.SDKUsage != nil {
			t.Fatalf("An insert changed a snapshotted group: %+v", group)
		}
	}
	fresh, err := storage.QueryTotals(t.Context(), "paged_totals", "configured", bounds)
	if err != nil || len(fresh.Groups) != requests+1 || fresh.Groups[0].ReportedModel != "0000-before" || fresh.Groups[1].ReportedModel != "0000-"+modelSuffix || fresh.Groups[1].Requests != 2 {
		t.Fatalf("A new snapshot must include committed inserts: count=%d error=%v", len(fresh.Groups), err)
	}
	history, err := storage.Query(t.Context(), "paged_totals", "configured", bounds)
	if err != nil || len(history.Events) != requests+2 {
		t.Fatalf("Read original SDK envelopes after totals pagination: count=%d error=%v", len(history.Events), err)
	}
	for _, event := range history.Events {
		if event.SDKUsage == nil || event.SDKUsage.UsageJSON != nativeJSON {
			t.Fatal("Totals pagination changed native SDK metadata.")
		}
	}
}

func totalsCounter(value int64) *int64 {
	return &value
}

func totalsPrice(t *testing.T, groups []quota.UsageTotalsGroup, writes *float64, wantCost float64, wantRequests int64) {
	t.Helper()
	pricing := config.ModelPricing{InputPerMillionTokens: 2, CachedInputPerMillionTokens: 0.2, CacheWritePerMillionTokens: writes, OutputPerMillionTokens: 8}
	var cost float64
	var requests int64
	for _, group := range groups {
		if group.InputTokens > 0 && (group.CachedInputTokens == nil || group.CacheWriteTokens == nil) {
			continue
		}
		var cached, written int64
		if group.CachedInputTokens != nil {
			cached = *group.CachedInputTokens
		}
		if group.CacheWriteTokens != nil {
			written = *group.CacheWriteTokens
		}
		estimate, priced := pricing.Estimate(group.InputTokens, cached, written, group.OutputTokens, group.ServiceTier)
		if priced {
			cost += estimate.TotalUSD()
			requests += group.Requests
		}
	}
	if math.Abs(cost-wantCost) > 1e-12 || requests != wantRequests {
		t.Fatalf("Grouping changed priced cost or request counts: cost=%f requests=%d", cost, requests)
	}
}

func totalsConfiguredPageBytes(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile("../../runtime.json")
	if err != nil {
		t.Fatal(err)
	}
	var configuration struct {
		PageBytes string `json:"PROVIDER_USAGE_PAGE_BYTES"`
	}
	if err := json.Unmarshal(data, &configuration); err != nil {
		t.Fatal(err)
	}
	maximumBytes, err := strconv.Atoi(configuration.PageBytes)
	if err != nil {
		t.Fatal(err)
	}
	return maximumBytes
}

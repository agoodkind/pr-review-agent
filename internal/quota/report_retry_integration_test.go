//go:build integration

package quota_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/quota"
)

func TestUsageReportRetriesAfterStorageCommitsWithoutDuplicatingCounters(t *testing.T) {
	t.Run("the next attempt receives acknowledgment", func(t *testing.T) {
		verifyReportDelivery(t, quota.DefaultReportPolicy(), 1, 2)
	})
	t.Run("three failed deliveries permit an explicit replay", func(t *testing.T) {
		verifyReportDelivery(t, quota.DefaultReportPolicy(), 3, 3)
	})
	t.Run("a configured fourth attempt receives acknowledgment", func(t *testing.T) {
		verifyReportDelivery(t, quota.ReportPolicy{Attempts: 4, AttemptTimeout: time.Second, Timeout: 5 * time.Second}, 3, 4)
	})
	t.Run("one configured attempt returns the delivery failure", func(t *testing.T) {
		verifyReportDelivery(t, quota.ReportPolicy{Attempts: 1, AttemptTimeout: time.Second, Timeout: 2 * time.Second}, 1, 1)
	})
}

func verifyReportDelivery(t *testing.T, policy quota.ReportPolicy, interruptions, expectedAttempts int64) {
	t.Helper()
	endpoint := startBudgetWorker(t)
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	path := target.Path
	target.Path = ""
	var responses atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.StatusCode == http.StatusOK && responses.Add(1) <= interruptions {
			if err := response.Body.Close(); err != nil {
				return err
			}
			return errors.New("the connection failed after the worker committed usage")
		}
		return nil
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, _ error) {
		connection, _, hijackErr := writer.(http.Hijacker).Hijack()
		if hijackErr != nil {
			t.Errorf("cannot interrupt the committed response: %v", hijackErr)
			return
		}
		if closeErr := connection.Close(); closeErr != nil {
			t.Errorf("cannot close the interrupted response: %v", closeErr)
		}
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	client, err := quota.NewReportingClient(server.URL+path, []byte("test-review-budget-secret"), nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	legacyTokens := int64(300)
	report := quota.ReportRequest{
		ReportID: "committed-response-lost", ProviderID: "retry_provider", Model: "retry_model",
		AdmittedAtMS: now.UnixMilli(),
		Usage:        quota.Usage{InputTokens: 200, OutputTokens: 100, TotalTokens: 300},
		LegacyDay:    now.Format("2006-01-02"), LegacyTokens: &legacyTokens,
	}
	reportErr := client.Report(t.Context(), report)
	if interruptions < expectedAttempts && reportErr != nil {
		t.Fatal(reportErr)
	}
	if interruptions == expectedAttempts && reportErr == nil {
		t.Fatal("the client acknowledged a report without receiving a response")
	}
	if responses.Load() != expectedAttempts {
		t.Fatalf("the client made %d attempts after losing the committed response", responses.Load())
	}
	storage := quota.NewClient(endpoint, []byte("test-review-budget-secret"), nil)
	if err := storage.Report(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	bounds := quota.Bounds{StartMS: now.Add(-time.Minute).UnixMilli(), EndMS: now.Add(time.Minute).UnixMilli(), IncludeStart: true, IncludeEnd: true}
	history, err := storage.Query(t.Context(), report.ProviderID, report.Model, bounds)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Events) != 1 || history.Events[0].Usage != report.Usage {
		t.Fatalf("storage duplicated or changed retried usage: %+v", history.Events)
	}
	daily, err := storage.CheckDaily(t.Context(), report.ProviderID, report.Model, 1000)
	if err != nil || daily.Used == nil || *daily.Used != legacyTokens {
		t.Fatalf("storage duplicated the legacy counter: %+v, %v", daily, err)
	}
	report.InputTokens++
	if err := storage.Report(t.Context(), report); err == nil {
		t.Fatal("storage accepted different usage with an existing report identity")
	}
	otherModel := report
	otherModel.Model = "other_model"
	if err := storage.Report(t.Context(), otherModel); err == nil {
		t.Fatal("storage accepted a different model with an existing report identity")
	}
	history, err = storage.Query(t.Context(), report.ProviderID, report.Model, bounds)
	if err != nil || len(history.Events) != 1 || history.Events[0].InputTokens != 200 {
		t.Fatalf("a rejected conflicting report changed usage: %+v, %v", history, err)
	}
}

func TestReportPolicyBoundsAttemptAndTotalDeliveryTime(t *testing.T) {
	endpoint := startBudgetWorker(t)
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	path := target.Path
	target.Path = ""
	var responses atomic.Int64
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(_ *http.Response) error {
		responses.Add(1)
		time.Sleep(250 * time.Millisecond)
		return nil
	}
	server := httptest.NewServer(proxy)
	defer server.Close()
	policy := quota.ReportPolicy{Attempts: 10, AttemptTimeout: 50 * time.Millisecond, Timeout: 130 * time.Millisecond}
	client, err := quota.NewReportingClient(server.URL+path, []byte("test-review-budget-secret"), nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = client.Report(t.Context(), quota.ReportRequest{ReportID: "delayed-acknowledgment", ProviderID: "deadline_provider", Model: "deadline_model", AdmittedAtMS: started.UnixMilli(), Usage: quota.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delayed delivery returned %v instead of its configured deadline", err)
	}
	if time.Since(started) >= 300*time.Millisecond || responses.Load() < 2 || responses.Load() > 3 {
		t.Fatalf("configured timeouts did not bound delivery: elapsed=%s attempts=%d", time.Since(started), responses.Load())
	}
	storage := quota.NewClient(endpoint, []byte("test-review-budget-secret"), nil)
	history, err := storage.Query(t.Context(), "deadline_provider", "deadline_model", quota.Bounds{StartMS: started.Add(-time.Minute).UnixMilli(), EndMS: started.Add(time.Minute).UnixMilli(), IncludeStart: true, IncludeEnd: true})
	if err != nil || len(history.Events) != 1 || history.Events[0].TotalTokens != 5 {
		t.Fatalf("delayed delivery did not preserve exactly one usage event: %+v, %v", history, err)
	}
}

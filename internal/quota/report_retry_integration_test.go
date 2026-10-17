//go:build integration

package quota_test

import (
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
		verifyReportDelivery(t, 1, 2)
	})
	t.Run("three failed deliveries permit an explicit replay", func(t *testing.T) {
		verifyReportDelivery(t, 3, 3)
	})
}

func verifyReportDelivery(t *testing.T, interruptions, expectedAttempts int64) {
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
	client := quota.NewClient(server.URL+path, []byte("test-review-budget-secret"), nil)
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

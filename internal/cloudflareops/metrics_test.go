package cloudflareops_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/domain"
)

func TestMetricsDistinguishRetriedExecutionsAndReconciliationOutcomes(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil)).With("delivery_id", "delivery", "repository", "example/repository", "pull_request", 7)
	logger.Info("webhook delivery accepted", "action", "synchronize", "head", "head")
	logger.Info("review job started")
	logger.Info("model provider attempt failed", "cause", "provider_rate_limited", "api_status", 429)
	logger.Info("model provider fallback engaged", "cause", "provider_rate_limited")
	logger.Info("review job failed", "err", "rate_limit_exceeded and a separate decode failure")
	logger.Info("review job started")
	logger.Info("review reconciliation selected", "thread_node_ids", fmt.Sprint([]string{"PRRT_selected"}))
	logger.Info("review reconciliation analysis completed", "resolutions", "[]", "resolved_threads", "[]")
	logger.Info("review reconciliation analysis completed", "resolutions", fmt.Sprint([]domain.ThreadResolution{{ThreadNodeID: "PRRT_decided", Resolution: domain.ResolutionOpen, Reason: "finding applies"}}), "resolved_threads", "[]")
	logger.Info("review reconciliation analysis completed", "resolutions", []domain.ThreadResolution{{ThreadNodeID: "PRRT_resolved", Resolution: domain.ResolutionResolved, Reason: "finding fixed"}}, "resolved_threads", []struct {
		NodeID string `json:"node_id"`
	}{{NodeID: "PRRT_resolved"}})
	logger.Info("review job left chunks pending", "pending", "2")
	logger.Info("review verdict unchanged", "review_id", "81", "event", "REQUEST_CHANGES")
	logger.Info("review job completed", "check_run_id", "91")
	logger.Info("review job skip completed", "check_run_id", "92")
	capturedAt := time.Now().UTC().Add(time.Minute)
	metrics := summarizeLogs(t, logs.Bytes(), capturedAt, true)
	if metrics.Total.StartedExecutions != 2 || metrics.Total.FailedExecutions != 1 || metrics.Total.Runs != 1 || metrics.Manifest.Duplicates != 1 {
		t.Fatalf("execution counts = %+v, duplicates = %d", metrics.Total, metrics.Manifest.Duplicates)
	}
	if metrics.Total.ProviderFailureRuns != 1 || metrics.Total.AttemptFailures != 1 || metrics.Total.FallbackEngagements != 1 {
		t.Fatalf("provider counts = %+v", metrics.Total)
	}
	if metrics.Total.IncompleteRuns != 1 || metrics.Total.CompletedRuns != 1 || metrics.Total.SkippedRuns != 1 {
		t.Fatalf("outcome counts = %+v", metrics.Total)
	}
	if metrics.Total.MeasuredInputTokens != nil || metrics.Total.MeasuredOutputTokens != nil || metrics.Total.UsageKnown {
		t.Fatal("missing usage must remain unknown")
	}
	if metrics.Total.FailureClasses["source_unknown"] != 1 || metrics.Total.FailureClasses["transient_rate_limit"] != 0 {
		t.Fatalf("joined fatal error classes = %v", metrics.Total.FailureClasses)
	}
	var decisions []string
	var resolved []string
	var emptyCompletions int
	for _, event := range metrics.Runs[0].Lifecycle {
		if !event.Timestamp.Before(capturedAt) {
			t.Fatal("lifecycle timestamp must use the original log timestamp")
		}
		if event.Message == "review reconciliation selected" && len(event.DecisionThreadIDs) != 0 {
			t.Fatal("thread selection must not count as a returned decision")
		}
		if event.Message == "review reconciliation analysis completed" {
			decisions = append(decisions, event.DecisionThreadIDs...)
			resolved = append(resolved, event.ResolvedThreadIDs...)
			if len(event.DecisionThreadIDs) == 0 {
				emptyCompletions++
			}
		}
		if event.Message == "review verdict unchanged" && (event.ReviewID != 81 || event.Event != "REQUEST_CHANGES") {
			t.Fatalf("verdict metadata = %+v", event)
		}
	}
	if fmt.Sprint(decisions) != "[PRRT_decided PRRT_resolved]" || fmt.Sprint(resolved) != "[PRRT_resolved]" || emptyCompletions != 1 {
		t.Fatalf("decisions = %v, resolved = %v, empty completions = %d", decisions, resolved, emptyCompletions)
	}
	encoded, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("separate decode failure")) || bytes.Contains(encoded, []byte("finding applies")) {
		t.Fatal("metrics must omit error payloads and reconciliation prose")
	}
}

func TestMetricsReadLegacyResolvedThreadsAndInvalidLogTime(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil)).With("delivery_id", "delivery", "repository", "example/repository")
	logger.Info("review reconciliation analysis completed", "time", "invalid", "resolutions", "[{PRRT_fixed resolved finding fixed}]", "resolved_threads", "[{PRRT_fixed 42}]")
	logger.Info("model provider attempt failed", "api_status", "429", "api_code", "insufficient_quota")
	capturedAt := time.Now().UTC()
	metrics := summarizeLogs(t, logs.Bytes(), capturedAt, false)
	completion := metrics.Runs[0].Lifecycle[0]
	if !completion.Timestamp.Equal(time.UnixMilli(capturedAt.UnixMilli()).UTC()) || fmt.Sprint(completion.DecisionThreadIDs) != "[PRRT_fixed]" || fmt.Sprint(completion.ResolvedThreadIDs) != "[PRRT_fixed]" {
		t.Fatalf("legacy completion = %+v", completion)
	}
	if len(metrics.Attempts) != 1 || metrics.Attempts[0].Class != "provider_api_exhausted" || metrics.Attempts[0].Evidence != "structured" {
		t.Fatalf("structured provider failure = %+v", metrics.Attempts)
	}
}

func TestMetricsAssignExecutionCountsToSourceDay(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil)).With("delivery_id", "delivery", "repository", "example/repository")
	capturedAt := time.Now().UTC()
	startedAt := capturedAt.AddDate(0, 0, -1)
	logger.Info("review job started", "time", startedAt.Format(time.RFC3339Nano))
	logger.Info("review job failed", "time", capturedAt.Format(time.RFC3339Nano))
	metrics := summarizeLogs(t, logs.Bytes(), capturedAt, false)
	startedDay := metrics.Days[startedAt.Format(time.DateOnly)]
	failedDay := metrics.Days[capturedAt.Format(time.DateOnly)]
	if startedDay.StartedExecutions != 1 || startedDay.FailedExecutions != 0 || failedDay.StartedExecutions != 0 || failedDay.FailedExecutions != 1 {
		t.Fatalf("source day counts = %+v; capture day counts = %+v", startedDay, failedDay)
	}
	if metrics.Total.StartedRuns != 1 || metrics.Total.FailedExecutionRuns != 1 || metrics.Total.StartedExecutions != 1 || metrics.Total.FailedExecutions != 1 {
		t.Fatalf("execution totals = %+v", metrics.Total)
	}
}

func summarizeLogs(t *testing.T, logs []byte, capturedAt time.Time, duplicate bool) cloudflareops.Metrics {
	t.Helper()
	var events bytes.Buffer
	encoder := json.NewEncoder(&events)
	scanner := bufio.NewScanner(bytes.NewReader(logs))
	var index int
	for scanner.Scan() {
		index++
		var source map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &source); err != nil {
			t.Fatal(err)
		}
		source["message"] = source[slog.MessageKey]
		delete(source, slog.MessageKey)
		event := cloudflareops.Event{
			Timestamp: capturedAt.UnixMilli(),
			Source:    source,
			Metadata:  map[string]json.RawMessage{"id": json.RawMessage(strconv.Quote(strconv.Itoa(index)))},
		}
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
		if duplicate && index == 1 {
			if err := encoder.Encode(event); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "events.ndjson")
	if err := os.WriteFile(path, events.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := cloudflareops.ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := cloudflareops.Summarize(input, cloudflareops.Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	return metrics
}

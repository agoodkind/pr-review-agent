package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/clock"
	"goodkind.io/pr-review-agent/internal/cloudflareops"
)

func TestCachedMetricsCountsDuplicateUnreadEventOnce(t *testing.T) {
	directory := t.TempDir()
	timestamp := clock.System().UTC()
	var event cloudflareops.Event
	event.Timestamp = timestamp.UnixMilli()
	event.Metadata = map[string]json.RawMessage{"id": json.RawMessage(strconv.Quote("duplicate-event")), "service": json.RawMessage(strconv.Quote("test-worker"))}
	event.Source = map[string]json.RawMessage{"message": json.RawMessage(strconv.Quote("review chunks unread")), "delivery_id": json.RawMessage(strconv.Quote("test-run")), "repository": json.RawMessage(strconv.Quote("example/repository")), "chunks_failed": json.RawMessage(strconv.Quote("2"))}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(directory, "events.ndjson")
	duplicate := append(append(append([]byte{}, encoded...), '\n'), encoded...)
	completed := event
	completed.Metadata = map[string]json.RawMessage{"id": json.RawMessage(strconv.Quote("completed-event")), "service": json.RawMessage(strconv.Quote("test-worker"))}
	completed.Source = map[string]json.RawMessage{"message": json.RawMessage(strconv.Quote("review model analysis completed")), "delivery_id": json.RawMessage(strconv.Quote("completed-run")), "repository": json.RawMessage(strconv.Quote("example/repository")), "coverage_complete": json.RawMessage(strconv.Quote("false")), "decision": json.RawMessage(strconv.Quote("APPROVE")), "chunks_failed": json.RawMessage(strconv.Quote("0"))}
	completedBytes, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	duplicate = append(append(duplicate, '\n'), completedBytes...)
	if err := os.WriteFile(input, duplicate, 0o600); err != nil {
		t.Fatal(err)
	}
	var manifest cloudflareops.Manifest
	manifest.From = timestamp.Add(-time.Minute)
	manifest.To = timestamp.Add(time.Minute)
	manifest.Complete = true
	manifest.Records = 3
	if err := cloudflareops.WriteJSON(filepath.Join(directory, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "output")
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"metrics", "--input", input, "--account", "test-account", "--script", "test-worker", "--output-dir", output}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(output, "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var results cloudflareops.Metrics
	if err := json.Unmarshal(data, &results); err != nil {
		t.Fatal(err)
	}
	if results.Total.Records != 2 || results.Total.Runs != 2 || results.Total.RunsWithFailureObserved != 1 || results.Manifest.Duplicates != 1 {
		t.Fatalf("CLI counted duplicate work: records=%d runs=%d failures=%d duplicates=%d", results.Total.Records, results.Total.Runs, results.Total.RunsWithFailureObserved, results.Manifest.Duplicates)
	}
	if results.Total.UsageKnown || results.Total.MeasuredInputTokens != nil || results.Total.MeasuredOutputTokens != nil {
		t.Fatal("CLI reported measured token use without a usage report")
	}
	if results.Total.FailureClasses["source_unknown"] != 1 {
		t.Fatal("CLI omitted the unclassified failed run")
	}
	if len(results.Runs) != 2 || results.Runs[1].FailedChunks != 2 {
		t.Fatal("CLI omitted the forwarded numeric chunk count")
	}
}

package cloudflareops

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// QuotaSnapshot separates app admission counters from provider API failures.
type QuotaSnapshot struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	Known           bool   `json:"known"`
	Used            int64  `json:"used"`
	Limit           int64  `json:"limit"`
	Remaining       int64  `json:"remaining"`
	WindowMode      string `json:"window_mode,omitempty"`
	WindowStartMS   int64  `json:"window_start_ms,omitempty"`
	WindowEndMS     int64  `json:"window_end_ms,omitempty"`
	AvailableAtMS   int64  `json:"available_at_ms,omitempty"`
	HistoryStartMS  int64  `json:"history_start_ms,omitempty"`
	HistoryComplete bool   `json:"history_complete"`
}

// Attempt stores classified metadata without provider error payloads.
type Attempt struct {
	Timestamp  time.Time      `json:"timestamp"`
	RunID      string         `json:"run_id,omitempty"`
	Repository string         `json:"repository,omitempty"`
	Provider   string         `json:"provider,omitempty"`
	Model      string         `json:"model,omitempty"`
	Class      string         `json:"class"`
	Evidence   string         `json:"evidence"`
	APIStatus  int64          `json:"api_status,omitempty"`
	APICode    string         `json:"api_code,omitempty"`
	Quota      *QuotaSnapshot `json:"quota,omitempty"`
}

// Run stores one aggregate usage summary per delivery identifier.
type Run struct {
	ID              string    `json:"id"`
	Repository      string    `json:"repository,omitempty"`
	PullRequest     int64     `json:"pull_request,omitempty"`
	Head            string    `json:"head,omitempty"`
	First           time.Time `json:"first"`
	Last            time.Time `json:"last"`
	Records         int       `json:"records"`
	FailureObserved bool      `json:"failure_observed"`
	Classes         []string  `json:"classes"`
	FailedChunks    int64     `json:"failed_chunks,omitempty"`
	UsageReported   bool      `json:"usage_reported"`
	InputTokens     int64     `json:"input_tokens,omitempty"`
	OutputTokens    int64     `json:"output_tokens,omitempty"`
}

// Counts uses null token totals when explicit measured usage is unavailable.
type Counts struct {
	Records                 int            `json:"records"`
	Runs                    int            `json:"runs"`
	RunsWithFailureObserved int            `json:"runs_with_failure_observed"`
	FailureClasses          map[string]int `json:"failure_classes"`
	AttemptClasses          map[string]int `json:"attempt_classes"`
	RunsReportingUsage      int            `json:"runs_reporting_usage"`
	UsageKnown              bool           `json:"usage_known"`
	MeasuredInputTokens     *int64         `json:"measured_input_tokens"`
	MeasuredOutputTokens    *int64         `json:"measured_output_tokens"`
}

// Metrics includes unclassified failures because absent provider metadata does not establish success.
type Metrics struct {
	Manifest       Manifest          `json:"manifest"`
	Total          Counts            `json:"total"`
	Days           map[string]Counts `json:"days"`
	Repositories   map[string]Counts `json:"repositories"`
	Runs           []Run             `json:"runs"`
	Attempts       []Attempt         `json:"attempts"`
	UnknownRecords int               `json:"records_without_run_id"`
	Limitations    []string          `json:"limitations"`
}

var legacyProvider = regexp.MustCompile(`provider ([A-Za-z0-9_-]+) model ([A-Za-z0-9_./:-]+)`)

func newCounts() Counts {
	var counts Counts
	counts.FailureClasses = make(map[string]int)
	counts.AttemptClasses = make(map[string]int)
	return counts
}

type providerCause string

const (
	appBudgetDenied        providerCause = "app_budget_denied"
	appBudgetUnavailable   providerCause = "app_budget_unavailable"
	providerUsageExhausted providerCause = "provider_usage_exhausted"
	providerRateLimited    providerCause = "provider_rate_limited"
	providerRequestFailed  providerCause = "provider_request_failed"
)

func classify(fields map[string]json.RawMessage) (string, string) {
	cause := providerCause(text(fields, "cause"))
	switch cause {
	case appBudgetDenied:
		return "app_cap", "structured"
	case appBudgetUnavailable:
		return "app_quota_unavailable", "structured"
	case providerUsageExhausted:
		return "provider_api_exhausted", "structured"
	case providerRateLimited:
		return "transient_rate_limit", "structured"
	case providerRequestFailed:
		if text(fields, "api_code") == "rate_limit_exceeded" {
			return "transient_rate_limit", "structured"
		}
		return "provider_request_failed", "structured"
	}
	errorText := strings.ToLower(text(fields, "err") + " " + text(fields, "error") + " " + text(fields, "causes"))
	app := strings.Contains(errorText, "daily token limit exhausted") || strings.Contains(errorText, "app token limit exhausted")
	api := strings.Contains(errorText, "insufficient_quota") || strings.Contains(errorText, "usage_limit_reached")
	if app && api {
		return "app_cap_and_provider_api_exhausted", "legacy_error_text"
	}
	if app {
		return "app_cap", "legacy_error_text"
	}
	if api {
		return "provider_api_exhausted", "legacy_error_text"
	}
	if strings.Contains(errorText, "rate_limit_exceeded") || strings.Contains(errorText, "too many requests") {
		return "transient_rate_limit", "legacy_error_text"
	}
	if strings.Contains(errorText, "refusal") || strings.Contains(errorText, "model refused") {
		return "model_refusal", "legacy_error_text"
	}
	return "source_unknown", "unclassified"
}

func failureRecord(message string) bool {
	return message == "review chunks unread" || strings.HasPrefix(message, "review stopped") || message == "refresh reviewed check" || message == "review job failed" || message == "refuse unsupported verdict"
}

func isAttempt(message string) bool {
	return message == "model provider attempt failed" || message == "model provider fallback engaged"
}

// Summarize deduplicates record IDs before counting failures by repository and delivery identifier.
func Summarize(events []Event, manifest Manifest) (Metrics, error) {
	var metrics Metrics
	metrics.Manifest = manifest
	metrics.Total = newCounts()
	metrics.Days = make(map[string]Counts)
	metrics.Repositories = make(map[string]Counts)
	metrics.Runs = []Run{}
	metrics.Attempts = []Attempt{}
	metrics.Limitations = []string{"Telemetry exhaustion does not prove full runtime log retention or unsampled history.", "Legacy error text classification is separate from structured provider attempts.", "Missing usage reports represent unknown usage; app-denied requests do not imply API token use.", "Only explicit run usage summaries are counted. Request and budget reports are excluded from run totals."}
	metrics.Limitations = append(metrics.Limitations, "Failure class counts overlap when one run contains failures from multiple classes.")
	collector := metricCollector{metrics: &metrics, seen: make(map[string]struct{}), runs: make(map[string]*Run)}
	for _, event := range events {
		if err := collector.record(event); err != nil {
			return metrics, err
		}
	}
	for _, run := range collector.runs {
		collector.finishRun(run)
	}
	sort.Slice(metrics.Runs, func(first, second int) bool { return metrics.Runs[first].ID < metrics.Runs[second].ID })
	metrics.Manifest.Records = metrics.Total.Records
	return metrics, nil
}

type metricCollector struct {
	metrics *Metrics
	seen    map[string]struct{}
	runs    map[string]*Run
}

func (collector *metricCollector) record(event Event) error {
	if event.Timestamp <= 0 {
		return errors.New("event input contains an invalid timestamp")
	}
	if boolean(event.Workers, "truncated") {
		return errors.New("event input contains a truncated log record")
	}
	manifest := collector.metrics.Manifest
	if manifest.Script != "" && event.script() != manifest.Script {
		collector.metrics.Manifest.Excluded++
		return nil
	}
	if (!manifest.From.IsZero() && event.Timestamp < manifest.From.UnixMilli()) || (!manifest.To.IsZero() && event.Timestamp > manifest.To.UnixMilli()) {
		return nil
	}
	identity, err := event.identity()
	if err != nil {
		return err
	}
	if _, exists := collector.seen[identity]; exists {
		collector.metrics.Manifest.Duplicates++
		return nil
	}
	collector.seen[identity] = struct{}{}
	collector.metrics.Total.Records++
	timestamp := time.UnixMilli(event.Timestamp).UTC()
	day := timestamp.Format(time.DateOnly)
	daily := collector.day(day)
	daily.Records++
	collector.metrics.Days[day] = daily
	repository := text(event.Source, "repository")
	if repository == "" {
		repository = "unidentified"
	}
	repositoryCounts := collector.repository(repository)
	repositoryCounts.Records++
	collector.metrics.Repositories[repository] = repositoryCounts
	id := text(event.Source, "delivery_id")
	if id == "" {
		id = text(event.Source, "run_id")
	}
	if id == "" {
		collector.metrics.UnknownRecords++
		return nil
	}
	run := collector.run(id, repository, timestamp)
	updateRun(run, event)
	if isAttempt(text(event.Source, "message")) {
		attempt := providerAttempt(event, run)
		collector.metrics.Attempts = append(collector.metrics.Attempts, attempt)
		collector.metrics.Total.AttemptClasses[attempt.Class]++
		daily.AttemptClasses[attempt.Class]++
		repositoryCounts.AttemptClasses[attempt.Class]++
	}
	collector.metrics.Days[day] = daily
	collector.metrics.Repositories[repository] = repositoryCounts
	return nil
}

func (collector *metricCollector) day(key string) Counts {
	counts, exists := collector.metrics.Days[key]
	if !exists {
		counts = newCounts()
	}
	return counts
}

func (collector *metricCollector) repository(key string) Counts {
	counts, exists := collector.metrics.Repositories[key]
	if !exists {
		counts = newCounts()
	}
	return counts
}

func (collector *metricCollector) run(id string, repository string, timestamp time.Time) *Run {
	key := repository + ":" + id
	run, exists := collector.runs[key]
	if !exists {
		var created Run
		run = &created
		run.ID = id
		run.Repository = repository
		run.First = timestamp
		run.Last = timestamp
		run.Classes = []string{}
		collector.runs[key] = run
	}
	return run
}

func updateRun(run *Run, event Event) {
	timestamp := time.UnixMilli(event.Timestamp).UTC()
	run.Records++
	if timestamp.Before(run.First) {
		run.First = timestamp
	}
	if timestamp.After(run.Last) {
		run.Last = timestamp
	}
	if value, ok := number(event.Source, "pull_request"); ok {
		run.PullRequest = value
	}
	if head := text(event.Source, "head"); head != "" {
		run.Head = head
	}
	message := text(event.Source, "message")
	if failureRecord(message) {
		run.FailureObserved = true
		class, _ := classify(event.Source)
		if !slices.Contains(run.Classes, class) {
			run.Classes = append(run.Classes, class)
		}
		if count, ok := number(event.Source, "chunks_failed"); ok {
			run.FailedChunks = max(run.FailedChunks, count)
		}
	}
	if message == "review usage reported" || message == "review model usage" || message == "review usage summary" {
		input, hasInput := number(event.Source, "input_tokens")
		output, hasOutput := number(event.Source, "output_tokens")
		if hasInput && hasOutput && input >= 0 && output >= 0 {
			run.UsageReported = true
			run.InputTokens = max(run.InputTokens, input)
			run.OutputTokens = max(run.OutputTokens, output)
		}
	}
}

func providerAttempt(event Event, run *Run) Attempt {
	var attempt Attempt
	attempt.Timestamp = time.UnixMilli(event.Timestamp).UTC()
	attempt.RunID = run.ID
	attempt.Repository = run.Repository
	attempt.Provider = text(event.Source, "provider_id")
	attempt.Model = text(event.Source, "configured_model")
	attempt.APICode = text(event.Source, "api_code")
	attempt.APIStatus, _ = number(event.Source, "api_status")
	attempt.Class, attempt.Evidence = classify(event.Source)
	if attempt.Evidence == "legacy_error_text" {
		match := legacyProvider.FindStringSubmatch(text(event.Source, "err") + " " + text(event.Source, "error"))
		if len(match) == 3 {
			attempt.Provider = match[1]
			attempt.Model = match[2]
		}
	}
	if attempt.Evidence == "structured" {
		attempt.Quota = quotaSnapshot(event.Source, attempt)
	}
	return attempt
}

func quotaSnapshot(source map[string]json.RawMessage, attempt Attempt) *QuotaSnapshot {
	var snapshot QuotaSnapshot
	snapshot.Provider = attempt.Provider
	snapshot.Model = attempt.Model
	snapshot.Known = boolean(source, "quota_known")
	snapshot.WindowMode = text(source, "quota_window_mode")
	snapshot.HistoryComplete = boolean(source, "quota_history_complete")
	snapshot.Used, _ = number(source, "quota_used")
	snapshot.Limit, _ = number(source, "quota_limit")
	snapshot.Remaining, _ = number(source, "quota_remaining")
	snapshot.WindowStartMS, _ = number(source, "quota_window_start_ms")
	snapshot.WindowEndMS, _ = number(source, "quota_window_end_ms")
	snapshot.AvailableAtMS, _ = number(source, "quota_available_at_ms")
	snapshot.HistoryStartMS, _ = number(source, "quota_history_start_ms")
	return &snapshot
}

func addRun(counts *Counts, run *Run) {
	counts.Runs++
	if run.FailureObserved {
		counts.RunsWithFailureObserved++
		for _, class := range run.Classes {
			counts.FailureClasses[class]++
		}
	}
	if run.UsageReported {
		counts.RunsReportingUsage++
		counts.UsageKnown = true
		if counts.MeasuredInputTokens == nil {
			var input, output int64
			counts.MeasuredInputTokens = &input
			counts.MeasuredOutputTokens = &output
		}
		*counts.MeasuredInputTokens += run.InputTokens
		*counts.MeasuredOutputTokens += run.OutputTokens
	}
}

func (collector *metricCollector) finishRun(run *Run) {
	addRun(&collector.metrics.Total, run)
	day := run.First.Format(time.DateOnly)
	daily := collector.day(day)
	addRun(&daily, run)
	collector.metrics.Days[day] = daily
	repositoryCounts := collector.repository(run.Repository)
	addRun(&repositoryCounts, run)
	collector.metrics.Repositories[run.Repository] = repositoryCounts
	collector.metrics.Runs = append(collector.metrics.Runs, *run)
}

// MarshalJSON uses a distinct wire type because encoding Metrics directly would recurse.
func (metrics Metrics) MarshalJSON() ([]byte, error) {
	type wireMetrics Metrics
	data, err := json.Marshal(wireMetrics(metrics))
	if err != nil {
		return nil, fmt.Errorf("encode metrics: %w", err)
	}
	return data, nil
}

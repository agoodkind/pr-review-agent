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
	Message    string         `json:"message"`
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
	ID                 string           `json:"id"`
	Repository         string           `json:"repository,omitempty"`
	PullRequest        int64            `json:"pull_request,omitempty"`
	Head               string           `json:"head,omitempty"`
	First              time.Time        `json:"first"`
	Last               time.Time        `json:"last"`
	Records            int              `json:"records"`
	FailureObserved    bool             `json:"failure_observed"`
	Classes            []string         `json:"classes"`
	FailedChunks       int64            `json:"failed_chunks,omitempty"`
	UsageReported      bool             `json:"usage_reported"`
	InputTokens        int64            `json:"input_tokens,omitempty"`
	OutputTokens       int64            `json:"output_tokens,omitempty"`
	StartedExecutions  int              `json:"started_executions"`
	FailedExecutions   int              `json:"failed_executions"`
	ProviderFailures   int              `json:"provider_failures"`
	IncompleteObserved bool             `json:"incomplete_observed"`
	CompletionObserved bool             `json:"completion_observed"`
	SkipObserved       bool             `json:"skip_observed"`
	Lifecycle          []LifecycleEvent `json:"lifecycle"`
}

// Counts uses null token totals when explicit measured usage is unavailable.
type Counts struct {
	Records                 int            `json:"records"`
	Runs                    int            `json:"runs"`
	RunsWithFailureObserved int            `json:"runs_with_failure_observed"`
	StartedExecutions       int            `json:"started_executions"`
	FailedExecutions        int            `json:"failed_executions"`
	StartedRuns             int            `json:"started_runs"`
	FailedExecutionRuns     int            `json:"failed_execution_runs"`
	ProviderFailureRuns     int            `json:"provider_failure_runs"`
	IncompleteRuns          int            `json:"incomplete_runs"`
	CompletedRuns           int            `json:"completed_runs"`
	SkippedRuns             int            `json:"skipped_runs"`
	AttemptFailures         int            `json:"attempt_failures"`
	FallbackEngagements     int            `json:"fallback_engagements"`
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

type providerAPICode string

const (
	apiCodeInsufficientQuota providerAPICode = "insufficient_quota"
	apiCodeUsageLimitReached providerAPICode = "usage_limit_reached"
	apiCodeRateLimitExceeded providerAPICode = "rate_limit_exceeded"
)

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
		if class := apiFailureClass(fields); class != "" {
			return class, "structured"
		}
		return "provider_request_failed", "structured"
	}
	if class := apiFailureClass(fields); class != "" {
		return class, "structured"
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
	if isAttempt(text(fields, "message")) && (strings.Contains(errorText, "rate_limit_exceeded") || strings.Contains(errorText, "too many requests")) {
		return "transient_rate_limit", "legacy_error_text"
	}
	if strings.Contains(errorText, "refusal") || strings.Contains(errorText, "model refused") {
		return "model_refusal", "legacy_error_text"
	}
	return "source_unknown", "unclassified"
}

func apiFailureClass(fields map[string]json.RawMessage) string {
	switch providerAPICode(text(fields, "api_code")) {
	case apiCodeInsufficientQuota, apiCodeUsageLimitReached:
		return "provider_api_exhausted"
	case apiCodeRateLimitExceeded:
		return "transient_rate_limit"
	}
	status, known := number(fields, "api_status")
	if known && status >= 400 && status <= 599 {
		return "provider_request_failed"
	}
	return ""
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
	metrics.Limitations = append(metrics.Limitations, "Execution counts use explicit job start and failure events. Run outcome counts use distinct delivery identifiers with observed outcomes and can overlap after retries.")
	metrics.Limitations = append(metrics.Limitations, "Daily run totals use each delivery's first captured day. Daily execution totals use source log dates when valid.")
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
		addAttempt(&collector.metrics.Total, attempt)
		addAttempt(&daily, attempt)
		addAttempt(&repositoryCounts, attempt)
	}
	collector.metrics.Days[day] = daily
	collector.metrics.Repositories[repository] = repositoryCounts
	collector.recordExecutionDay(event)
	return nil
}

func (collector *metricCollector) recordExecutionDay(event Event) {
	message := text(event.Source, "message")
	if message != "review job started" && message != "review job failed" {
		return
	}
	day := sourceTimestamp(event).Format(time.DateOnly)
	counts := collector.day(day)
	if message == "review job started" {
		counts.StartedExecutions++
	}
	if message == "review job failed" {
		counts.FailedExecutions++
	}
	collector.metrics.Days[day] = counts
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
		run.Lifecycle = []LifecycleEvent{}
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
	updateLifecycle(run, event)
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
	attempt.Timestamp = sourceTimestamp(event)
	attempt.Message = text(event.Source, "message")
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
	if run.StartedExecutions > 0 {
		counts.StartedRuns++
	}
	if run.FailedExecutions > 0 {
		counts.FailedExecutionRuns++
	}
	if run.ProviderFailures > 0 {
		counts.ProviderFailureRuns++
	}
	if run.IncompleteObserved {
		counts.IncompleteRuns++
	}
	if run.CompletionObserved {
		counts.CompletedRuns++
	}
	if run.SkipObserved {
		counts.SkippedRuns++
	}
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

func addAttempt(counts *Counts, attempt Attempt) {
	if attempt.Message == "model provider attempt failed" {
		counts.AttemptFailures++
	}
	if attempt.Message == "model provider fallback engaged" {
		counts.FallbackEngagements++
	}
}

func (collector *metricCollector) finishRun(run *Run) {
	sort.SliceStable(run.Lifecycle, func(first, second int) bool {
		return run.Lifecycle[first].Timestamp.Before(run.Lifecycle[second].Timestamp)
	})
	addRun(&collector.metrics.Total, run)
	collector.metrics.Total.StartedExecutions += run.StartedExecutions
	collector.metrics.Total.FailedExecutions += run.FailedExecutions
	day := run.First.Format(time.DateOnly)
	daily := collector.day(day)
	addRun(&daily, run)
	collector.metrics.Days[day] = daily
	repositoryCounts := collector.repository(run.Repository)
	addRun(&repositoryCounts, run)
	repositoryCounts.StartedExecutions += run.StartedExecutions
	repositoryCounts.FailedExecutions += run.FailedExecutions
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

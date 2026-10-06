package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"goodkind.io/pr-review-agent/internal/opsstats"
)

func statsField(value string) string {
	if value == "" {
		return "unknown"
	}
	return strings.Join(strings.Fields(value), " ")
}

func statsTokens(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatInt(*value, 10)
}

func printStats(output io.Writer, report opsstats.Report, path string) error {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	sections := []func(io.Writer, opsstats.Report) error{writeStatsWindow, writeStatsCounts, writeStatsFailures, writeStatsGitHub, writeStatsRevisits, writeStatsQuota, writeStatsQueue}
	for _, section := range sections {
		if err := section(writer, report); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(writer, "Private report with delivery IDs and GitHub evidence: %s\n", path); err != nil {
		return err
	}
	return writer.Flush()
}

func writeStatsWindow(output io.Writer, report opsstats.Report) error {
	manifest := report.Metrics.Manifest
	if _, err := fmt.Fprintf(output, "Requested interval: %s through %s.\nCounts cover observed telemetry. Log retention and sampling remain unverified.\n", manifest.From.Format(time.RFC3339), manifest.To.Format(time.RFC3339)); err != nil {
		return err
	}
	if report.ObservedLifecycleFrom == nil || report.ObservedLifecycleTo == nil {
		return nil
	}
	_, err := fmt.Fprintf(output, "Observed lifecycle interval: %s through %s.\n", report.ObservedLifecycleFrom.Format(time.RFC3339), report.ObservedLifecycleTo.Format(time.RFC3339))
	return err
}

func writeStatsCounts(output io.Writer, report opsstats.Report) error {
	counts := report.Metrics.Total
	if _, err := fmt.Fprintf(output, "\nObserved deliveries\tStarted executions\tFailed executions\tDeliveries with provider failures\tIncomplete deliveries\n%d\t%d\t%d\t%d\t%d\n", counts.Runs, counts.StartedExecutions, counts.FailedExecutions, counts.ProviderFailureRuns, counts.IncompleteRuns); err != nil {
		return err
	}
	_, err := fmt.Fprintf(output, "Completed deliveries: %d. Skipped deliveries: %d. Provider failures: %d. Fallback events: %d.\nMeasured input tokens: %s. Measured output tokens: %s.\n", counts.CompletedRuns, counts.SkippedRuns, counts.AttemptFailures, counts.FallbackEngagements, statsTokens(counts.MeasuredInputTokens), statsTokens(counts.MeasuredOutputTokens))
	return err
}

type statsAttemptKey struct{ provider, model, class string }

func statsAttemptCounts(report opsstats.Report) (map[statsAttemptKey]int, []statsAttemptKey) {
	counts := make(map[statsAttemptKey]int)
	for _, attempt := range report.Metrics.Attempts {
		if attempt.Message == "model provider attempt failed" {
			counts[statsAttemptKey{provider: attempt.Provider, model: attempt.Model, class: attempt.Class}]++
		}
	}
	keys := make([]statsAttemptKey, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		return keys[left].provider+"\x00"+keys[left].model+"\x00"+keys[left].class < keys[right].provider+"\x00"+keys[right].model+"\x00"+keys[right].class
	})
	return counts, keys
}

func writeStatsFailures(output io.Writer, report opsstats.Report) error {
	counts, keys := statsAttemptCounts(report)
	if len(keys) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(output, "\nFailed provider\tRequested model\tFailure category\tAttempts"); err != nil {
		return err
	}
	for _, key := range keys {
		if _, err := fmt.Fprintf(output, "%s\t%s\t%s\t%d\n", statsField(key.provider), statsField(key.model), statsField(key.class), counts[key]); err != nil {
			return err
		}
	}
	return nil
}

func writeStatsGitHub(output io.Writer, report opsstats.Report) error {
	if _, err := fmt.Fprintf(output, "\nGitHub capture completed at %s. Draft pull requests: %d.\n", report.CapturedAt.Format(time.RFC3339), report.Totals.DraftPullRequests); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "\nOpen pull request\tDraft\tBot rejections\tOld commit rejections\tUnresolved bot threads"); err != nil {
		return err
	}
	for _, pr := range report.PullRequests {
		if pr.Excluded || pr.State != "OPEN" || (pr.StandingBotRejections == 0 && pr.UnresolvedBotThreads == 0) {
			continue
		}
		if _, err := fmt.Fprintf(output, "%s\t%t\t%d\t%d\t%d\n", pr.Target.String(), pr.Draft, pr.StandingBotRejections, pr.OldHeadBotRejections, pr.UnresolvedBotThreads); err != nil {
			return err
		}
	}
	return nil
}

func writeStatsRevisits(output io.Writer, report opsstats.Report) error {
	totals := report.Totals
	if _, err := fmt.Fprintf(output, "Open reviews with later admission and no recorded reassessment: %d.\nOpen threads with later admission and no recorded decision: %d.\nReassessed open threads that remain unresolved: %d.\nItems with unknown terminal state or an active check: %d.\nClosed or merged pull requests contain %d unresolved bot threads.\n", totals.OpenReviewsWithoutRecordedReassessment, totals.OpenThreadsWithoutRecordedDecision, totals.OpenThreadsReassessedUnresolved, totals.OpenItemsWithoutTerminalEvidence, totals.HistoricalUnresolvedThreads); err != nil {
		return err
	}
	if totals.HistoricalReviewsWithUnknownState == 0 {
		return nil
	}
	_, err := fmt.Fprintf(output, "Historical standing states are unknown for %d review IDs; private evidence records their later admitted deliveries.\n", totals.HistoricalReviewsWithUnknownState)
	return err
}

func writeStatsQuota(output io.Writer, report opsstats.Report) error {
	if report.QuotaCapturePath != "" {
		if _, err := fmt.Fprintf(output, "Private current quota snapshots: %s\n", report.QuotaCapturePath); err != nil {
			return err
		}
	}
	if report.QuotaUnavailableReason == "" {
		return nil
	}
	_, err := fmt.Fprintln(output, report.QuotaUnavailableReason)
	return err
}

func writeStatsQueue(output io.Writer, report opsstats.Report) error {
	queue := report.CurrentQueue
	if queue == nil {
		return nil
	}
	if !queue.Known {
		_, err := fmt.Fprintf(output, "Current reassessment queue is unavailable. HTTP status: %d.\n", queue.HTTPStatus)
		return err
	}
	phases := make(map[string]int)
	for _, record := range queue.Records {
		phases[record.Phase]++
	}
	if _, err := fmt.Fprintf(output, "Current reassessment queue has %d waiting records, %d running records, and %d terminal records.\n", phases["waiting"], phases["running"], phases["terminal"]); err != nil {
		return err
	}
	for _, write := range []func(io.Writer, *opsstats.QueueSnapshot) error{writeRegistryDiagnostics, writeSweepDiagnostics, writeAlarmDiagnostics, writeTransitionDiagnostics} {
		if err := write(output, queue); err != nil {
			return err
		}
	}
	return nil
}

func writeRegistryDiagnostics(output io.Writer, queue *opsstats.QueueSnapshot) error {
	if !queue.RegistryKnown {
		_, err := fmt.Fprintln(output, "Registry diagnostics are unavailable.")
		return err
	}
	privateOutcomes, eligibleKnown := 0, 0
	for _, target := range queue.Targets {
		if target.Version > 0 && target.PrivateNonce != "" && target.PrivateNonce == target.PrivateOutcome.Nonce {
			privateOutcomes++
		}
		if target.Observation.EligibilityKnown {
			eligibleKnown++
		}
	}
	_, err := fmt.Fprintf(output, "The registry contains %d targets. %d targets include versioned private outcomes. %d targets record known eligibility.\n", len(queue.Targets), privateOutcomes, eligibleKnown)
	return err
}

func writeSweepDiagnostics(output io.Writer, queue *opsstats.QueueSnapshot) error {
	if !queue.SweepKnown {
		_, err := fmt.Fprintln(output, "Inventory cursor diagnostics are unavailable.")
		return err
	}
	if queue.Sweep == nil {
		_, err := fmt.Fprintln(output, "The inventory sweep has no stored cursor.")
		return err
	}
	cursor := queue.Sweep
	if _, err := fmt.Fprintf(output, "Inventory stage\tCursor version\tInstallation page/index\tRepository page/index\tPull request page/index\n%d\t%d\t%d/%d\t%d/%d\t%d/%d\n", cursor.Stage, cursor.Version, cursor.InstallationPage, cursor.InstallationIndex, cursor.RepositoryPage, cursor.RepositoryIndex, cursor.PullRequestPage, cursor.PullRequestIndex); err != nil {
		return err
	}
	_, err := fmt.Fprintln(output, "Inventory failure history is unavailable.")
	return err
}

func writeAlarmDiagnostics(output io.Writer, queue *opsstats.QueueSnapshot) error {
	if !queue.AlarmKnown {
		_, err := fmt.Fprintln(output, "The next alarm time is unavailable.")
		return err
	}
	if queue.NextAlarmMS == nil {
		_, err := fmt.Fprintln(output, "The Worker has no scheduled alarm.")
		return err
	}
	_, err := fmt.Fprintf(output, "The next alarm is scheduled for %s.\n", time.UnixMilli(*queue.NextAlarmMS).UTC().Format(time.RFC3339))
	return err
}

func writeTransitionDiagnostics(output io.Writer, queue *opsstats.QueueSnapshot) error {
	if !queue.TransitionMetadataKnown {
		_, err := fmt.Fprintln(output, "Accepted transition history is unavailable.")
		return err
	}
	if queue.TransitionCounts == nil {
		_, err := fmt.Fprintln(output, "The Worker has not initialized transition counters.")
		return err
	}
	counts := queue.TransitionCounts
	if _, err := fmt.Fprintf(output, "Accepted transition counters cover %s through %s.\nTransition\tAccepted count\n", time.UnixMilli(counts.SinceMS).UTC().Format(time.RFC3339), time.UnixMilli(counts.LastUpdateMS).UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	keys := make([]string, 0, len(counts.Counts))
	for key := range counts.Counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := fmt.Fprintf(output, "%s\t%d\n", statsField(key), counts.Counts[key]); err != nil {
			return err
		}
	}
	return nil
}

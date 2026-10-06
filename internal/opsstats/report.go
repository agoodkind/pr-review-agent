package opsstats

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/reassessment"
)

// Proof identifies an observed operation without publishing its private payload.
type Proof struct {
	RunID     string    `json:"run_id"`
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
	Head      string    `json:"head,omitempty"`
}

// QueueSnapshot distinguishes an unavailable endpoint from an empty queue.
type QueueSnapshot struct {
	CapturedAt              time.Time                 `json:"captured_at"`
	Known                   bool                      `json:"known"`
	HTTPStatus              int                       `json:"http_status,omitempty"`
	UnavailableReason       string                    `json:"unavailable_reason,omitempty"`
	Records                 []reassessment.Record     `json:"records,omitempty"`
	RegistryKnown           bool                      `json:"registry_known"`
	Targets                 []reassessment.Target     `json:"targets,omitempty"`
	SweepKnown              bool                      `json:"sweep_known"`
	Sweep                   *reassessment.SweepCursor `json:"sweep"`
	AlarmKnown              bool                      `json:"alarm_known"`
	NextAlarmMS             *int64                    `json:"next_alarm_ms"`
	TransitionMetadataKnown bool                      `json:"transition_metadata_known"`
	TransitionCounts        *QueueTransitionCounts    `json:"transition_counts"`
	SweepFailureKnown       bool                      `json:"sweep_failure_known"`
	UnknownFields           []string                  `json:"unknown_fields,omitempty"`
}

// QueueTransitionCounts covers accepted CAS transitions within the recorded counter epoch.
type QueueTransitionCounts struct {
	Counts       map[string]int64 `json:"counts"`
	SinceMS      int64            `json:"since_ms"`
	LastUpdateMS int64            `json:"last_update_ms"`
}

// Trigger counts an admitted delivery separately from repeated job executions.
type Trigger struct {
	RunID                    string    `json:"run_id"`
	Timestamp                time.Time `json:"timestamp"`
	Action                   string    `json:"action"`
	Label                    string    `json:"label,omitempty"`
	Head                     string    `json:"head,omitempty"`
	CurrentHead              bool      `json:"current_head"`
	StartedExecutions        int       `json:"started_executions"`
	FailedExecutions         int       `json:"failed_executions"`
	FailureObserved          bool      `json:"failure_observed"`
	ExecutionStatus          string    `json:"latest_execution_status"`
	CheckRunIDs              []int64   `json:"check_run_ids"`
	CurrentCheckStatus       string    `json:"current_check_status,omitempty"`
	CurrentCheckConclusion   string    `json:"current_check_conclusion,omitempty"`
	ReassessmentReturned     bool      `json:"reassessment_returned_by_delivery"`
	HeadReassessmentReturned bool      `json:"head_reassessment_returned_after_admission"`
}

// Revisit separates current standing state from incomplete historical evidence.
type Revisit struct {
	CurrentReviewState       string    `json:"current_review_state,omitempty"`
	HistoricalStanding       string    `json:"historical_standing,omitempty"`
	StandingReview           bool      `json:"standing_review,omitempty"`
	WindowRevisitStatus      string    `json:"observed_window_revisit_status,omitempty"`
	QueueRecordIDs           []string  `json:"current_queue_record_ids,omitempty"`
	Target                   Target    `json:"target"`
	Kind                     string    `json:"kind"`
	ID                       string    `json:"id"`
	ReviewID                 int64     `json:"review_id,omitempty"`
	CreatedAt                time.Time `json:"created_at"`
	TriggerBoundary          time.Time `json:"trigger_boundary"`
	TriggerBoundarySource    string    `json:"trigger_boundary_source"`
	PullRequestState         string    `json:"pull_request_state"`
	Draft                    bool      `json:"draft"`
	Resolved                 bool      `json:"resolved"`
	OldHead                  bool      `json:"old_head"`
	FindingHeadKnown         bool      `json:"finding_head_known"`
	FindingHead              string    `json:"finding_head,omitempty"`
	Importance               *int      `json:"importance,omitempty"`
	MeetsImportanceThreshold *bool     `json:"meets_current_importance_threshold,omitempty"`
	BlocksThreadResolution   *bool     `json:"blocks_required_thread_resolution,omitempty"`
	Status                   string    `json:"status"`
	Triggers                 []Trigger `json:"later_admitted_triggers"`
	Reassessments            []Proof   `json:"returned_reassessments"`
	SelectedWithoutDecision  []Proof   `json:"selected_without_returned_decision"`
}

// PullRequestSummary excludes other bots from PR-Agent rejection counts.
type PullRequestSummary struct {
	Target                Target  `json:"target"`
	State                 string  `json:"state"`
	Draft                 bool    `json:"draft"`
	Excluded              bool    `json:"excluded"`
	StandingBotRejections int     `json:"standing_bot_rejections"`
	OldHeadBotRejections  int     `json:"old_head_bot_rejections"`
	UnresolvedBotThreads  int     `json:"unresolved_bot_threads"`
	BotChecks             []Check `json:"bot_checks"`
}

// Totals excludes resolved findings and closed pull requests from current counts.
type Totals struct {
	HistoricalReviewsWithUnknownState      int `json:"historical_reviews_with_unknown_prior_state"`
	HistoricalRejectingReviews             int `json:"historical_rejecting_reviews_with_window_activity"`
	CapturedPullRequests                   int `json:"captured_pull_requests"`
	ExcludedPullRequests                   int `json:"excluded_pull_requests"`
	OpenPullRequests                       int `json:"open_pull_requests"`
	DraftPullRequests                      int `json:"draft_pull_requests"`
	OpenItemsWithoutTerminalEvidence       int `json:"open_items_with_later_admission_without_terminal_evidence"`
	OpenBotRejections                      int `json:"open_bot_rejections"`
	OpenOldHeadRejections                  int `json:"open_old_head_rejections"`
	OpenUnresolvedBotThreads               int `json:"open_unresolved_bot_threads"`
	OpenReviewsWithoutRecordedReassessment int `json:"open_reviews_with_later_admission_without_recorded_reassessment"`
	OpenThreadsWithoutRecordedDecision     int `json:"open_threads_with_later_admission_without_recorded_decision"`
	OpenThreadsReassessedUnresolved        int `json:"open_threads_reassessed_unresolved"`
	HistoricalUnresolvedThreads            int `json:"closed_or_merged_unresolved_bot_threads"`
}

// Report records observation times because GitHub state can change after the requested window.
type Report struct{ reportData }

type reportData struct {
	QuotaUnavailableReason string                `json:"quota_unavailable_reason,omitempty"`
	CurrentQueue           *QueueSnapshot        `json:"current_reassessment_queue,omitempty"`
	CapturedAt             time.Time             `json:"captured_at"`
	Owner                  string                `json:"owner"`
	BotLogin               string                `json:"bot_login"`
	Excluded               []string              `json:"excluded"`
	Metrics                cloudflareops.Metrics `json:"metrics"`
	GitHubComplete         bool                  `json:"github_connections_complete"`
	GitHub                 []PullRequest         `json:"github"`
	Totals                 Totals                `json:"totals"`
	PullRequests           []PullRequestSummary  `json:"pull_requests"`
	Revisits               []Revisit             `json:"revisits"`
	Limitations            []string              `json:"limitations"`
	QuotaCapturePath       string                `json:"current_quota_capture_path,omitempty"`
	CurrentQuota           json.RawMessage       `json:"current_quota_snapshots,omitempty"`
	ObservedLifecycleFrom  *time.Time            `json:"observed_lifecycle_from,omitempty"`
	ObservedLifecycleTo    *time.Time            `json:"observed_lifecycle_to,omitempty"`
}

// MarshalJSON encodes tagged evidence fields for private artifact writes.
func (report *Report) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal(report.reportData)
	if err != nil {
		return nil, fmt.Errorf("encode statistics report: %w", err)
	}
	return data, nil
}

func botAuthor(author *Actor, login string) bool {
	return author != nil && strings.TrimSuffix(author.Login, "[bot]") == strings.TrimSuffix(login, "[bot]")
}

// LogTargets supplements GitHub search with pull requests present in retained telemetry.
func LogTargets(metrics cloudflareops.Metrics) []Target {
	var targets []Target
	for _, run := range metrics.Runs {
		if run.PullRequest > 0 && strings.Count(run.Repository, "/") == 1 {
			targets = append(targets, Target{Repository: run.Repository, Number: run.PullRequest})
		}
	}
	return targets
}

func proof(run cloudflareops.Run, event cloudflareops.LifecycleEvent) Proof {
	head := event.Head
	if head == "" {
		head = run.Head
	}
	return Proof{RunID: run.ID, Timestamp: event.Timestamp, Message: event.Message, Head: head}
}

func missingReassessment(status string) bool {
	return strings.HasPrefix(status, "later_admitted_") && strings.HasSuffix(status, "_without_recorded_reassessment")
}

func unavailableTerminal(status string) bool {
	return status == "later_admitted_trigger_without_terminal_evidence" || status == "current_head_review_in_progress"
}

// Build uses explicit admissions and returned decisions; summary updates cannot prove a revisit.
func Build(metrics cloudflareops.Metrics, prs []PullRequest, owner, bot string, exclusions []string, minimumImportance int, capturedAt time.Time) Report {
	var report Report
	report.CapturedAt, report.Owner, report.BotLogin = capturedAt, owner, bot
	report.Excluded, report.Metrics, report.GitHub = exclusions, metrics, prs
	report.GitHubComplete = true
	report.Limitations = reportLimitations()
	report.Totals.CapturedPullRequests = len(prs)
	report.recordLifecycleBounds()
	for _, pr := range prs {
		report.recordPullRequest(pr, minimumImportance)
	}
	return report
}

func reportLimitations() []string {
	return []string{
		"GitHub publication timestamps have second precision. Matching publication logs establish a trigger boundary; otherwise triggers in the publication second remain unclassified.",
		"Available telemetry exhaustion does not prove complete retention or unsampled logs. Missing reassessments are unknown outside the observed records.",
		"Admitted deliveries and repeated job executions are counted separately. Returned thread decisions and explicit verdict reassessments establish revisits; summary writes do not.",
		"GitHub search uses current update timestamps in the requested window. Log targets supplement search results. Activity followed by a later update outside the window may be absent without telemetry.",
		"Current GitHub state is captured after the requested telemetry window. Resolved threads and closed or merged pull requests are excluded from current blocking counts.",
		"Failure provider metadata identifies the failed attempt, not the provider that created an earlier finding. Missing creation attribution remains unknown.",
		"Active ruleset requirements establish required thread resolution. Classic branch protection is not captured; absent ruleset requirements do not prove a thread cannot block merging.",
		"Dismissed reviews retain their current state and identifiers. The report does not reconstruct an original verdict or dismissal time.",
	}
}

func (report *Report) recordLifecycleBounds() {
	for _, run := range report.Metrics.Runs {
		for _, event := range run.Lifecycle {
			timestamp := event.Timestamp
			if report.ObservedLifecycleFrom == nil || timestamp.Before(*report.ObservedLifecycleFrom) {
				report.ObservedLifecycleFrom = &timestamp
			}
			if report.ObservedLifecycleTo == nil || timestamp.After(*report.ObservedLifecycleTo) {
				report.ObservedLifecycleTo = &timestamp
			}
		}
	}
}

func (report *Report) recordPullRequest(pr PullRequest, minimumImportance int) {
	var summary PullRequestSummary
	summary.Target = Target{Repository: pr.Repository.NameWithOwner, Number: pr.Number}
	summary.State, summary.Draft = pr.State, pr.IsDraft
	summary.Excluded = slices.Contains(report.Excluded, summary.Target.String())
	summary.BotChecks = botChecks(pr, report.BotLogin)
	if summary.Excluded {
		report.Totals.ExcludedPullRequests++
		report.PullRequests = append(report.PullRequests, summary)
		return
	}
	runs := report.runsFor(summary.Target)
	if pr.State == "OPEN" {
		report.Totals.OpenPullRequests++
	}
	if pr.State == "OPEN" && pr.IsDraft {
		report.Totals.DraftPullRequests++
	}
	report.recordStandingReviews(pr, &summary, runs)
	report.recordHistoricalReviews(pr, summary, runs)
	report.recordThreads(pr, &summary, runs, minimumImportance)
	report.PullRequests = append(report.PullRequests, summary)
}

func (report *Report) runsFor(target Target) []cloudflareops.Run {
	var runs []cloudflareops.Run
	for _, run := range report.Metrics.Runs {
		if run.Repository == target.Repository && run.PullRequest == target.Number {
			runs = append(runs, run)
		}
	}
	return runs
}

func botChecks(pr PullRequest, bot string) []Check {
	checks := []Check{}
	if pr.StatusCheckRollup == nil {
		return checks
	}
	for _, check := range pr.StatusCheckRollup.Contexts.Nodes {
		if check.CheckSuite.App.Slug == strings.TrimSuffix(bot, "[bot]") {
			checks = append(checks, check)
		}
	}
	return checks
}

func newReviewRevisit(pr PullRequest, review Review, target Target) Revisit {
	var item Revisit
	item.Target, item.Kind, item.ID = target, "review", review.ID
	item.ReviewID, item.CreatedAt = review.DatabaseID, review.SubmittedAt
	item.CurrentReviewState = review.State
	item.PullRequestState, item.Draft = pr.State, pr.IsDraft
	item.OldHead = review.Commit.OID != pr.HeadRefOID
	return item
}

func (report *Report) recordStandingReviews(pr PullRequest, summary *PullRequestSummary, runs []cloudflareops.Run) {
	for _, review := range pr.LatestOpinionatedReviews.Nodes {
		if review.State != "CHANGES_REQUESTED" || !botAuthor(review.Author, report.BotLogin) {
			continue
		}
		item := newReviewRevisit(pr, review, summary.Target)
		item.StandingReview = true
		correlate(&item, runs, pr.HeadRefOID, summary.BotChecks, report.Metrics.Manifest.To)
		summary.StandingBotRejections++
		if item.OldHead {
			summary.OldHeadBotRejections++
		}
		report.Revisits = append(report.Revisits, item)
		report.recordCurrentItem(item)
	}
}

func isStanding(pr PullRequest, id int64) bool {
	return slices.ContainsFunc(pr.LatestOpinionatedReviews.Nodes, func(review Review) bool { return review.DatabaseID == id })
}

func (report *Report) recordHistoricalReviews(pr PullRequest, summary PullRequestSummary, runs []cloudflareops.Run) {
	candidates := historicalCandidates(pr, runs, report.BotLogin)
	for _, review := range pr.Reviews.Nodes {
		if !candidates[review.DatabaseID] || isStanding(pr, review.DatabaseID) {
			continue
		}
		item := newReviewRevisit(pr, review, summary.Target)
		item.PullRequestState, item.Draft = "OPEN", false
		correlate(&item, runs, pr.HeadRefOID, summary.BotChecks, report.Metrics.Manifest.To)
		if len(item.Triggers) == 0 {
			continue
		}
		item.WindowRevisitStatus = item.Status
		item.PullRequestState, item.Draft = pr.State, pr.IsDraft
		item.HistoricalStanding = "unknown_dismissal_timing"
		item.Status = "historical_review_state_unknown"
		if review.State == "CHANGES_REQUESTED" {
			item.Status = "superseded_review"
			report.Totals.HistoricalRejectingReviews++
		} else {
			report.Totals.HistoricalReviewsWithUnknownState++
		}
		report.Revisits = append(report.Revisits, item)
	}
}

func latestOpinionBefore(reviews []Review, bot string, timestamp time.Time) *Review {
	var latest *Review
	for index := range reviews {
		review := &reviews[index]
		if !botAuthor(review.Author, bot) || review.SubmittedAt.After(timestamp) {
			continue
		}
		if review.State == "COMMENTED" || review.State == "PENDING" {
			continue
		}
		if latest == nil || review.SubmittedAt.After(latest.SubmittedAt) {
			latest = review
		}
	}
	return latest
}

func historicalCandidates(pr PullRequest, runs []cloudflareops.Run, bot string) map[int64]bool {
	candidates := make(map[int64]bool)
	for _, run := range runs {
		for _, event := range run.Lifecycle {
			if event.Message != "webhook delivery accepted" {
				continue
			}
			latest := latestOpinionBefore(pr.Reviews.Nodes, bot, event.Timestamp)
			if latest == nil {
				continue
			}
			if latest.State == "CHANGES_REQUESTED" || latest.State == "DISMISSED" {
				candidates[latest.DatabaseID] = true
			}
		}
	}
	return candidates
}

func (report *Report) recordThreads(pr PullRequest, summary *PullRequestSummary, runs []cloudflareops.Run, minimumImportance int) {
	for _, thread := range pr.ReviewThreads.Nodes {
		if len(thread.Comments.Nodes) == 0 || !botAuthor(thread.Comments.Nodes[0].Author, report.BotLogin) {
			continue
		}
		item := newThreadRevisit(pr, thread, summary.Target, minimumImportance)
		correlate(&item, runs, pr.HeadRefOID, summary.BotChecks, report.Metrics.Manifest.To)
		if !thread.IsResolved {
			summary.UnresolvedBotThreads++
		}
		report.Revisits = append(report.Revisits, item)
		report.recordCurrentItem(item)
	}
}

func (report *Report) recordCurrentItem(item Revisit) {
	if item.Resolved {
		return
	}
	if item.PullRequestState != "OPEN" {
		if item.Kind == "thread" {
			report.Totals.HistoricalUnresolvedThreads++
		}
		return
	}
	if item.Kind == "review" {
		report.Totals.OpenBotRejections++
	} else {
		report.Totals.OpenUnresolvedBotThreads++
	}
	if item.Kind == "review" && item.OldHead {
		report.Totals.OpenOldHeadRejections++
	}
	if item.Kind == "thread" && item.Status == "reassessed_unresolved" {
		report.Totals.OpenThreadsReassessedUnresolved++
	}
	report.recordMissingItem(item)
}

func (report *Report) recordMissingItem(item Revisit) {
	if item.PullRequestState != "OPEN" || item.Resolved || item.Draft {
		return
	}
	if unavailableTerminal(item.Status) {
		report.Totals.OpenItemsWithoutTerminalEvidence++
	}
	if !missingReassessment(item.Status) {
		return
	}
	if item.Kind == "review" {
		report.Totals.OpenReviewsWithoutRecordedReassessment++
	} else {
		report.Totals.OpenThreadsWithoutRecordedDecision++
	}
}

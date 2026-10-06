package cloudflareops

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
)

type operationMessage string

const (
	messageWebhookAccepted         operationMessage = "webhook delivery accepted"
	messageWebhookRejected         operationMessage = "webhook delivery rejected"
	messageWebhookSuppressed       operationMessage = "webhook delivery suppressed"
	messageCheckAdmitted           operationMessage = "review check admitted"
	messageCheckLoaded             operationMessage = "review check loaded"
	messageJobStarted              operationMessage = "review job started"
	messageJobFailed               operationMessage = "review job failed"
	messageJobCompleted            operationMessage = "review job completed"
	messageJobSkipped              operationMessage = "review job skipped"
	messageJobSkipCompleted        operationMessage = "review job skip completed"
	messageJobSuppressed           operationMessage = "review job suppressed"
	messageJobPending              operationMessage = "review job left chunks pending"
	messageChunksUnread            operationMessage = "review chunks unread"
	messageReconciliationSelected  operationMessage = "review reconciliation selected"
	messageReconciliationStarted   operationMessage = "review reconciliation analysis started"
	messageReconciliationCompleted operationMessage = "review reconciliation analysis completed"
	messageReviewPublished         operationMessage = "review published"
	messageVerdictRefreshed        operationMessage = "review verdict refreshed"
	messageVerdictUnchanged        operationMessage = "review verdict unchanged"
	messageRefreshSkipped          operationMessage = "verdict refresh skipped"
	messageRefreshWithheld         operationMessage = "verdict refresh withheld"
	messageSummaryCreated          operationMessage = "summary comment created"
	messageSummaryUpdated          operationMessage = "summary comment updated"
	messageFailureSummary          operationMessage = "failure summary written"
	messageProviderAttemptFailed   operationMessage = "model provider attempt failed"
)

// LifecycleEvent excludes comment bodies and provider error payloads.
type LifecycleEvent struct {
	Timestamp         time.Time `json:"timestamp"`
	Message           string    `json:"message"`
	Head              string    `json:"head,omitempty"`
	Action            string    `json:"action,omitempty"`
	Label             string    `json:"label,omitempty"`
	Event             string    `json:"event,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	Status            string    `json:"status,omitempty"`
	Conclusion        string    `json:"conclusion,omitempty"`
	ReviewID          int64     `json:"review_id,omitempty"`
	CheckRunID        int64     `json:"check_run_id,omitempty"`
	CommentID         int64     `json:"comment_id,omitempty"`
	Pending           *int64    `json:"pending,omitempty"`
	Visible           *bool     `json:"visible,omitempty"`
	SelectedThreadIDs []string  `json:"selected_thread_ids"`
	DecisionThreadIDs []string  `json:"decision_thread_ids"`
	ResolvedThreadIDs []string  `json:"resolved_thread_ids"`
}

var (
	legacyResolution     = regexp.MustCompile(`\{(PRRT_[A-Za-z0-9_-]+) (resolved|open|uncertain) `)
	legacyResolvedThread = regexp.MustCompile(`\{(PRRT_[A-Za-z0-9_-]+) [0-9]+\}`)
)

func sourceTimestamp(event Event) time.Time {
	if timestamp, err := time.Parse(time.RFC3339Nano, text(event.Source, "time")); err == nil && timestamp.UnixMilli() > 0 {
		return timestamp.UTC()
	}
	return time.UnixMilli(event.Timestamp).UTC()
}

func lifecycleMessage(message operationMessage) bool {
	switch message {
	case messageWebhookAccepted, messageWebhookRejected, messageWebhookSuppressed,
		messageCheckAdmitted, messageCheckLoaded, messageJobStarted, messageJobFailed,
		messageJobCompleted, messageJobSkipped, messageJobSkipCompleted, messageJobSuppressed,
		messageJobPending, messageChunksUnread,
		messageReconciliationSelected, messageReconciliationStarted, messageReconciliationCompleted,
		messageReviewPublished, messageVerdictRefreshed, messageVerdictUnchanged,
		messageRefreshSkipped, messageRefreshWithheld,
		messageSummaryCreated, messageSummaryUpdated, messageFailureSummary:
		return true
	case messageProviderAttemptFailed:
		return false
	}
	return false
}

func updateLifecycle(run *Run, event Event) {
	message := operationMessage(text(event.Source, "message"))
	switch message {
	case messageJobStarted:
		run.StartedExecutions++
	case messageJobFailed:
		run.FailedExecutions++
	case messageProviderAttemptFailed:
		run.ProviderFailures++
	case messageJobPending, messageChunksUnread:
		run.IncompleteObserved = true
	case messageJobCompleted:
		run.CompletionObserved = true
	case messageJobSkipped, messageJobSkipCompleted:
		run.SkipObserved = true
	case messageWebhookAccepted, messageWebhookRejected, messageWebhookSuppressed,
		messageCheckAdmitted, messageCheckLoaded, messageJobSuppressed,
		messageReconciliationSelected, messageReconciliationStarted, messageReconciliationCompleted,
		messageReviewPublished, messageVerdictRefreshed, messageVerdictUnchanged,
		messageRefreshSkipped, messageRefreshWithheld,
		messageSummaryCreated, messageSummaryUpdated, messageFailureSummary:
	}
	if !lifecycleMessage(message) {
		return
	}
	lifecycle := LifecycleEvent{
		Timestamp:         sourceTimestamp(event),
		Message:           string(message),
		Head:              text(event.Source, "head"),
		Action:            text(event.Source, "action"),
		Label:             text(event.Source, "label"),
		Event:             text(event.Source, "event"),
		Reason:            text(event.Source, "reason"),
		Status:            text(event.Source, "status"),
		Conclusion:        text(event.Source, "conclusion"),
		ReviewID:          0,
		CheckRunID:        0,
		CommentID:         0,
		Pending:           nil,
		Visible:           nil,
		SelectedThreadIDs: []string{},
		DecisionThreadIDs: []string{},
		ResolvedThreadIDs: []string{},
	}
	lifecycle.ReviewID, _ = number(event.Source, "review_id")
	lifecycle.CheckRunID, _ = number(event.Source, "check_run_id")
	lifecycle.CommentID, _ = number(event.Source, "comment_id")
	if pending, ok := number(event.Source, "pending"); ok {
		lifecycle.Pending = &pending
	}
	if visible, ok := booleanValue(event.Source, "visible"); ok {
		lifecycle.Visible = &visible
	}
	if message == messageReconciliationSelected || message == messageReconciliationStarted {
		lifecycle.SelectedThreadIDs = selectedThreadIDs(event.Source)
	}
	if message == messageReconciliationCompleted {
		lifecycle.DecisionThreadIDs = returnedDecisionIDs(event.Source)
		lifecycle.ResolvedThreadIDs = resolvedThreadIDs(event.Source)
	}
	run.Lifecycle = append(run.Lifecycle, lifecycle)
}

func selectedThreadIDs(fields map[string]json.RawMessage) []string {
	var identifiers []string
	if json.Unmarshal(fields["thread_node_ids"], &identifiers) == nil && identifiers != nil {
		return distinctIDs(identifiers)
	}
	encoded := text(fields, "thread_node_ids")
	if json.Unmarshal([]byte(encoded), &identifiers) == nil && identifiers != nil {
		return distinctIDs(identifiers)
	}
	if !strings.HasPrefix(encoded, "[") || !strings.HasSuffix(encoded, "]") {
		return []string{}
	}
	return distinctIDs(strings.Fields(strings.TrimSuffix(strings.TrimPrefix(encoded, "["), "]")))
}

func returnedDecisionIDs(fields map[string]json.RawMessage) []string {
	var decisions []struct {
		ThreadNodeID string            `json:"thread_node_id"`
		Resolution   domain.Resolution `json:"resolution"`
	}
	identifiers := []string{}
	raw := fields["resolutions"]
	encoded := text(fields, "resolutions")
	if json.Unmarshal(raw, &decisions) == nil || json.Unmarshal([]byte(encoded), &decisions) == nil {
		for _, decision := range decisions {
			switch decision.Resolution {
			case domain.ResolutionOpen, domain.ResolutionResolved, domain.ResolutionUncertain:
				identifiers = append(identifiers, decision.ThreadNodeID)
			}
		}
		return distinctIDs(identifiers)
	}
	for _, match := range legacyResolution.FindAllStringSubmatch(encoded, -1) {
		identifiers = append(identifiers, match[1])
	}
	return distinctIDs(identifiers)
}

func resolvedThreadIDs(fields map[string]json.RawMessage) []string {
	var threads []struct {
		NodeID string `json:"node_id"`
	}
	identifiers := []string{}
	raw := fields["resolved_threads"]
	encoded := text(fields, "resolved_threads")
	if json.Unmarshal(raw, &threads) == nil || json.Unmarshal([]byte(encoded), &threads) == nil {
		for _, thread := range threads {
			identifiers = append(identifiers, thread.NodeID)
		}
		return distinctIDs(identifiers)
	}
	for _, match := range legacyResolvedThread.FindAllStringSubmatch(encoded, -1) {
		identifiers = append(identifiers, match[1])
	}
	return distinctIDs(identifiers)
}

func distinctIDs(identifiers []string) []string {
	result := []string{}
	for _, identifier := range identifiers {
		if identifier != "" && !slices.Contains(result, identifier) {
			result = append(result, identifier)
		}
	}
	return result
}

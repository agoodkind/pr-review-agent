package opsstats

import (
	"slices"
	"sort"
	"time"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/marker"
)

type executionMessage string

const (
	jobStarted       executionMessage = "review job started"
	jobFailed        executionMessage = "review job failed"
	jobCompleted     executionMessage = "review job completed"
	jobSkipped       executionMessage = "review job skipped"
	jobSkipCompleted executionMessage = "review job skip completed"
	jobIncomplete    executionMessage = "review job left chunks pending"
)

func updateExecution(trigger *Trigger, message string) {
	switch executionMessage(message) {
	case jobStarted:
		trigger.StartedExecutions++
		trigger.ExecutionStatus = "terminal_unknown"
	case jobFailed:
		trigger.FailedExecutions++
		trigger.ExecutionStatus = "failed"
	case jobCompleted:
		trigger.ExecutionStatus = "completed"
	case jobSkipped, jobSkipCompleted:
		trigger.ExecutionStatus = "skipped"
	case jobIncomplete:
		trigger.ExecutionStatus = "incomplete"
	}
}

func triggerFromRun(run cloudflareops.Run, created time.Time, head string) (Trigger, bool) {
	var trigger Trigger
	trigger.RunID, trigger.FailureObserved = run.ID, run.FailureObserved
	trigger.ExecutionStatus = "terminal_unknown"
	for _, event := range run.Lifecycle {
		if !event.Timestamp.After(created) {
			continue
		}
		if event.CheckRunID > 0 && !slices.Contains(trigger.CheckRunIDs, event.CheckRunID) {
			trigger.CheckRunIDs = append(trigger.CheckRunIDs, event.CheckRunID)
		}
		updateExecution(&trigger, event.Message)
		if event.Message != "webhook delivery accepted" {
			continue
		}
		if !trigger.Timestamp.IsZero() && !event.Timestamp.Before(trigger.Timestamp) {
			continue
		}
		trigger.Timestamp, trigger.Action, trigger.Label, trigger.Head = event.Timestamp, event.Action, event.Label, event.Head
	}
	if trigger.Head == "" {
		trigger.Head = run.Head
	}
	trigger.CurrentHead = trigger.Head != "" && trigger.Head == head
	return trigger, !trigger.Timestamp.IsZero()
}

func triggersFor(runs []cloudflareops.Run, created time.Time, head string) []Trigger {
	triggers := []Trigger{}
	for _, run := range runs {
		trigger, admitted := triggerFromRun(run, created, head)
		if admitted {
			triggers = append(triggers, trigger)
		}
	}
	sort.Slice(triggers, func(left, right int) bool { return triggers[left].Timestamp.Before(triggers[right].Timestamp) })
	return triggers
}

func publicationBoundary(item *Revisit, runs []cloudflareops.Run) {
	item.TriggerBoundary = item.CreatedAt.Add(time.Second)
	item.TriggerBoundarySource = "github_timestamp_second_upper_bound"
	if item.Kind != "review" {
		return
	}
	for _, run := range runs {
		for _, event := range run.Lifecycle {
			if event.Message != "review published" || event.ReviewID != item.ReviewID || event.Timestamp.Before(item.CreatedAt) {
				continue
			}
			if item.TriggerBoundarySource != "publication_log" || event.Timestamp.Before(item.TriggerBoundary) {
				item.TriggerBoundary = event.Timestamp
				item.TriggerBoundarySource = "publication_log"
			}
		}
	}
}

func joinTriggerChecks(trigger *Trigger, checks []Check, end time.Time) {
	for _, check := range checks {
		if !slices.Contains(trigger.CheckRunIDs, check.DatabaseID) && check.ExternalID != trigger.RunID {
			continue
		}
		trigger.CurrentCheckStatus, trigger.CurrentCheckConclusion = check.Status, check.Conclusion
		if check.Status != "COMPLETED" {
			trigger.ExecutionStatus = "active"
			continue
		}
		if check.CompletedAt == nil || check.CompletedAt.After(end) || trigger.ExecutionStatus != "terminal_unknown" {
			continue
		}
		trigger.ExecutionStatus = "nonpassing_check"
		if check.Conclusion == "SUCCESS" {
			trigger.ExecutionStatus = "completed"
		}
	}
}

func hasReturnedThreadDecision(run cloudflareops.Run, thread string, selected time.Time) bool {
	return slices.ContainsFunc(run.Lifecycle, func(event cloudflareops.LifecycleEvent) bool {
		return !event.Timestamp.Before(selected) && slices.Contains(event.DecisionThreadIDs, thread)
	})
}

func recordThreadReassessment(item *Revisit, run cloudflareops.Run, event cloudflareops.LifecycleEvent) {
	if slices.Contains(event.DecisionThreadIDs, item.ID) || slices.Contains(event.ResolvedThreadIDs, item.ID) {
		item.Reassessments = append(item.Reassessments, proof(run, event))
	}
	if slices.Contains(event.SelectedThreadIDs, item.ID) && !hasReturnedThreadDecision(run, item.ID, event.Timestamp) {
		item.SelectedWithoutDecision = append(item.SelectedWithoutDecision, proof(run, event))
	}
}

func verdictReassessment(item Revisit, event cloudflareops.LifecycleEvent) bool {
	if event.Message == "review verdict refreshed" || event.Message == "review verdict unchanged" {
		return true
	}
	return event.Message == "review published" && event.ReviewID > 0 && event.ReviewID != item.ReviewID
}

func collectReassessments(item *Revisit, runs []cloudflareops.Run) {
	item.Reassessments, item.SelectedWithoutDecision = []Proof{}, []Proof{}
	for _, run := range runs {
		for _, event := range run.Lifecycle {
			if !event.Timestamp.After(item.CreatedAt) {
				continue
			}
			if item.Kind == "thread" {
				recordThreadReassessment(item, run, event)
				continue
			}
			if verdictReassessment(*item, event) {
				item.Reassessments = append(item.Reassessments, proof(run, event))
			}
		}
	}
}

func annotateCoverage(item *Revisit, head string) {
	for _, evidence := range item.Reassessments {
		if evidence.Head != head {
			continue
		}
		for index := range item.Triggers {
			trigger := &item.Triggers[index]
			if evidence.Timestamp.Before(trigger.Timestamp) {
				continue
			}
			trigger.HeadReassessmentReturned = true
			if evidence.RunID == trigger.RunID {
				trigger.ReassessmentReturned = true
			}
		}
	}
}

func lastCurrentTrigger(triggers []Trigger) *Trigger {
	var latest *Trigger
	for index := range triggers {
		trigger := &triggers[index]
		if !trigger.CurrentHead {
			continue
		}
		if latest == nil || trigger.Timestamp.After(latest.Timestamp) {
			latest = trigger
		}
	}
	return latest
}

func revisitStatus(item Revisit, head string) string {
	if item.Resolved {
		return "resolved"
	}
	if item.PullRequestState != "OPEN" {
		return "historical_closed_or_merged"
	}
	if item.Draft {
		return "draft"
	}
	if len(item.Triggers) == 0 {
		return "no_observed_later_admitted_trigger"
	}
	latest := lastCurrentTrigger(item.Triggers)
	if latest == nil {
		return "current_head_not_proven_in_later_admitted_trigger"
	}
	for _, evidence := range item.Reassessments {
		if !evidence.Timestamp.Before(latest.Timestamp) && evidence.Head == head {
			return "reassessed_unresolved"
		}
	}
	if latest.ExecutionStatus == "active" {
		return "current_head_review_in_progress"
	}
	if latest.ExecutionStatus == "terminal_unknown" {
		return "later_admitted_trigger_without_terminal_evidence"
	}
	return "later_admitted_" + latest.ExecutionStatus + "_without_recorded_reassessment"
}

func correlate(item *Revisit, runs []cloudflareops.Run, head string, checks []Check, end time.Time) {
	publicationBoundary(item, runs)
	item.Triggers = triggersFor(runs, item.TriggerBoundary, head)
	for index := range item.Triggers {
		joinTriggerChecks(&item.Triggers[index], checks, end)
	}
	collectReassessments(item, runs)
	annotateCoverage(item, head)
	item.Status = revisitStatus(*item, head)
}

func requiredThreadResolution(pr PullRequest, thread Thread) *bool {
	blocked := false
	if pr.State != "OPEN" || thread.IsResolved {
		return &blocked
	}
	if !pr.BranchRules.Known {
		return nil
	}
	for _, rule := range pr.BranchRules.Rules {
		if rule.Type == "pull_request" && rule.Parameters.RequiresThreadResolution {
			blocked = true
			return &blocked
		}
	}
	return nil
}

func newThreadRevisit(pr PullRequest, thread Thread, target Target, minimumImportance int) Revisit {
	var item Revisit
	root := thread.Comments.Nodes[0]
	item.Target, item.Kind, item.ID = target, "thread", thread.ID
	item.CreatedAt, item.PullRequestState = root.CreatedAt, pr.State
	item.Draft, item.Resolved = pr.IsDraft, thread.IsResolved
	item.BlocksThreadResolution = requiredThreadResolution(pr, thread)
	finding, found := marker.FindFinding(root.Body)
	if !found {
		return item
	}
	importance := finding.Importance
	eligible := minimumImportance > 0 && importance >= minimumImportance
	item.Importance, item.MeetsImportanceThreshold = &importance, &eligible
	item.FindingHead, item.FindingHeadKnown = string(finding.Head), true
	item.OldHead = string(finding.Head) != pr.HeadRefOID
	return item
}

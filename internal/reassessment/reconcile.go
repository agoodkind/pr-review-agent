package reassessment

import (
	"context"
	"crypto/rand"
	"log/slog"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// LiveSnapshot distinguishes validated absence from failed GitHub reads.
type LiveSnapshot struct {
	PullRequest  githubapp.PullRequest
	Check        githubapp.CheckRun
	CheckFound   bool
	Summary      marker.State
	SummaryFound bool
	FailureStage string
}

// Reconcile reads live state before proposing a version-checked tracking repair.
func (coordinator *Coordinator) Reconcile(ctx context.Context, target Target) Reconciliation {
	now := coordinator.Clock()
	response, err := coordinator.Store.Send(ctx, Mutation{Action: "query", Key: target.Key, ExpectedVersion: 0, Record: nil})
	if err != nil {
		var snapshot LiveSnapshot
		snapshot.FailureStage = "queue_read"
		plan := coordinator.Repair(target, nil, snapshot, now)
		plan.QueueVersionKnown = false
		return plan
	}
	snapshot := coordinator.ReadLive(ctx, target.Job)
	return coordinator.Repair(target, response.Record, snapshot, now)
}

// ReadLive keeps transport and malformed-state failures separate from empty results.
func (coordinator *Coordinator) ReadLive(ctx context.Context, job domain.ReviewJob) LiveSnapshot {
	var snapshot LiveSnapshot
	pullRequest, err := coordinator.GitHub.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		coordinator.Logger.WarnContext(ctx, "read tracked pull request", slog.String("err", err.Error()))
		snapshot.FailureStage = "pull_request"
		return snapshot
	}
	snapshot.PullRequest = pullRequest
	if !pullRequest.EligibilityKnown || !pullRequest.MetadataKnown {
		snapshot.FailureStage = "pull_request_shape"
		return snapshot
	}
	if pullRequest.State != "open" || pullRequest.Merged || pullRequest.Draft {
		return snapshot
	}
	check, found, err := coordinator.GitHub.FindCheckRun(ctx, job.InstallationID, job.Repository, pullRequest.Head, config.ReviewCheckName)
	if err != nil {
		coordinator.Logger.WarnContext(ctx, "read tracked review check", slog.String("err", err.Error()))
		snapshot.FailureStage = "check"
		return snapshot
	}
	snapshot.Check = check
	snapshot.CheckFound = found
	comments, err := coordinator.GitHub.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		coordinator.Logger.WarnContext(ctx, "read tracked checkpoints", slog.String("err", err.Error()))
		snapshot.FailureStage = "summary"
		return snapshot
	}
	for _, comment := range comments {
		if comment.Author != coordinator.BotLogin || !marker.HasState(comment.Body) {
			continue
		}
		state, valid := marker.DecodeState(comment.Body)
		if !valid {
			snapshot.FailureStage = "summary_shape"
			return snapshot
		}
		snapshot.Summary = state
		snapshot.SummaryFound = true
		break
	}
	return snapshot
}

// Repair preserves pending work when live evidence is unavailable or contradictory.
func (coordinator *Coordinator) Repair(target Target, record *Record, snapshot LiveSnapshot, now time.Time) Reconciliation {
	var plan Reconciliation
	plan.Key = target.Key
	plan.QueueVersionKnown = true
	plan.ExpectedTargetVersion = target.Version
	plan.Target = target
	plan.Target.NotBeforeMS = coordinator.nextReconciliation(now).UnixMilli()
	if record != nil {
		plan.ExpectedQueueVersion = record.Version
	}
	observation := observeLive(snapshot)
	if snapshot.FailureStage != "" || !observation.EligibilityKnown {
		return coordinator.deferRepair(plan, observation, now, "deferred")
	}
	coordinator.confirmObservation(&plan.Target, observation, now)
	pr := snapshot.PullRequest
	if pr.State != "open" || pr.Merged || pr.Draft {
		return coordinator.repairIneligible(plan, record, pr, now)
	}
	if completed, handled := coordinator.repairCompletion(plan, record, snapshot, now); handled {
		return completed
	}
	if !unfinishedAssessment(target, record, snapshot) {
		plan.RepairKind = "no_unfinished_evidence"
		return plan
	}
	if plan.Target.Confirmations < coordinator.Settings.StateConfirmations {
		return coordinator.confirmRepair(plan, now)
	}
	if record != nil && record.Phase != "terminal" && record.Head == pr.Head && (record.MetadataRevision == "" || record.MetadataRevision == diff.PullRequestRevision(pr)) {
		plan.RepairKind = "consistent_pending"
		return plan
	}
	job := target.Job
	job.DeliveryID = "reconciliation-" + rand.Text()
	renewed, _ := coordinator.Reactivate(record, job, pr, now)
	plan.Record = &renewed
	plan.RepairKind = "missing_queue"
	if record != nil {
		if record.Phase == "terminal" {
			plan.RepairKind = "reactivated"
		} else {
			plan.RepairKind = "mismatched_head"
		}
	}
	return plan
}

func (coordinator *Coordinator) repairCompletion(plan Reconciliation, record *Record, snapshot LiveSnapshot, now time.Time) (Reconciliation, bool) {
	target := plan.Target
	pr := snapshot.PullRequest
	if completionConfirmed(target, snapshot) {
		if plan.Target.Confirmations < coordinator.Settings.StateConfirmations {
			return coordinator.confirmRepair(plan, now), true
		}
		if record != nil && (record.Phase != "terminal" || record.Reason != "completed") {
			updated := coordinator.terminal(*record, "completed", now)
			updated.Head = pr.Head
			updated.MetadataRevision = diff.PullRequestRevision(pr)
			plan.Record = &updated
			plan.RepairKind = "completed"
		} else {
			plan.RepairKind = "consistent_completed"
		}
		return plan, true
	}
	if record != nil && record.RebuildEvidence && record.Phase != "terminal" {
		plan.RepairKind = "consistent_pending"
		return plan, true
	}
	if snapshot.CheckFound && snapshot.Check.Outcome.Disposition == domain.AssessmentCompleted || target.PrivateOutcome.Disposition == domain.AssessmentCompleted && target.PrivateOutcome.Head == pr.Head && target.PrivateOutcome.MetadataRevision == diff.PullRequestRevision(pr) {
		if plan.Target.Confirmations < coordinator.Settings.ReadBudget {
			return coordinator.confirmRepair(plan, now), true
		}
		if record != nil && record.Phase == "running" && record.Outcome.Disposition != domain.AssessmentCompleted {
			plan.RepairKind = "consistent_pending"
			return plan, true
		}
		job := target.Job
		job.DeliveryID = "evidence-rebuild-" + rand.Text()
		rebuilt, _ := coordinator.Reactivate(record, job, pr, now)
		rebuilt.RebuildEvidence = true
		rebuilt.Reason = "evidence_rebuild"
		plan.Record = &rebuilt
		plan.RepairKind = "evidence_rebuild"
		return plan, true
	}
	return plan, false
}

func observeLive(snapshot LiveSnapshot) Observation {
	var observation Observation
	pr := snapshot.PullRequest
	observation.EligibilityKnown = pr.EligibilityKnown && pr.MetadataKnown && (pr.State != "open" || !pr.Merged)
	observation.State = pr.State
	observation.Merged = pr.Merged
	observation.Draft = pr.Draft
	observation.Head = pr.Head
	observation.MetadataRevision = diff.PullRequestRevision(pr)
	observation.CheckFound = snapshot.CheckFound
	observation.CheckID = snapshot.Check.ID
	observation.CheckStatus = snapshot.Check.Status
	observation.Disposition = snapshot.Check.Outcome.Disposition
	observation.OutcomeNonce = snapshot.Check.Outcome.Nonce
	observation.SummaryFound = snapshot.SummaryFound
	observation.SummaryStatus = snapshot.Summary.Status
	observation.LastReviewed = snapshot.Summary.LastReviewed
	observation.Pending = len(snapshot.Summary.Pending)
	observation.Unread = len(snapshot.Summary.Unread)
	observation.FailureStage = snapshot.FailureStage
	return observation
}

func (coordinator *Coordinator) confirmObservation(target *Target, observation Observation, now time.Time) {
	interval, _ := time.ParseDuration(coordinator.Settings.ConfirmationInterval)
	if target.Observation != observation {
		target.Confirmations = 1
		target.ObservedAtMS = now.UnixMilli()
	} else if now.UnixMilli()-target.ObservedAtMS >= interval.Milliseconds() {
		target.Confirmations = min(target.Confirmations+1, max(coordinator.Settings.StateConfirmations, coordinator.Settings.ReadBudget))
		target.ObservedAtMS = now.UnixMilli()
	}
	target.Observation = observation
	target.Failures = 0
}

func (coordinator *Coordinator) deferRepair(plan Reconciliation, observation Observation, now time.Time, kind string) Reconciliation {
	plan.Target.Observation = observation
	plan.Target.Confirmations = 0
	plan.Target.Failures++
	plan.Target.ObservedAtMS = now.UnixMilli()
	plan.Target.NotBeforeMS = coordinator.delayed(now, plan.Target.Failures).UnixMilli()
	plan.RepairKind = kind
	return plan
}

func (coordinator *Coordinator) confirmRepair(plan Reconciliation, now time.Time) Reconciliation {
	interval, _ := time.ParseDuration(coordinator.Settings.ConfirmationInterval)
	plan.Target.NotBeforeMS = now.Add(interval).UnixMilli()
	plan.RepairKind = "confirming"
	return plan
}

func (coordinator *Coordinator) repairIneligible(plan Reconciliation, record *Record, pr githubapp.PullRequest, now time.Time) Reconciliation {
	if plan.Target.Confirmations < coordinator.Settings.StateConfirmations {
		return coordinator.confirmRepair(plan, now)
	}
	if record == nil {
		plan.RepairKind = "consistent_ineligible"
		return plan
	}
	reason := "closed"
	if pr.Merged {
		reason = "merged"
	} else if pr.Draft {
		reason = "draft"
	}
	if record.Phase == "terminal" && record.Reason == reason {
		plan.RepairKind = "consistent_ineligible"
		return plan
	}
	updated := coordinator.terminal(*record, reason, now)
	plan.Record = &updated
	plan.RepairKind = reason
	return plan
}

func completionConfirmed(target Target, snapshot LiveSnapshot) bool {
	pr := snapshot.PullRequest
	check := snapshot.Check
	private := target.PrivateOutcome
	return snapshot.CheckFound && snapshot.SummaryFound && snapshot.Summary.Status == marker.StateDone && snapshot.Summary.LastReviewed == pr.Head && len(snapshot.Summary.Pending) == 0 && private.Disposition == domain.AssessmentCompleted && private.Head == pr.Head && private.MetadataRevision == diff.PullRequestRevision(pr) && target.PrivateNonce != "" && private.Nonce == target.PrivateNonce && check.ID == private.CheckRunID && check.Status == "completed" && check.Outcome.Nonce == target.PrivateNonce && completedAt(check, pr.Head, private.MetadataRevision)
}

func unfinishedAssessment(target Target, record *Record, snapshot LiveSnapshot) bool {
	if record != nil && record.Phase != "terminal" {
		return true
	}
	private := target.PrivateOutcome
	if private.Disposition == domain.AssessmentFailed || private.Disposition == domain.AssessmentIncomplete {
		return true
	}
	if snapshot.CheckFound && (snapshot.Check.Outcome.Disposition == domain.AssessmentFailed || snapshot.Check.Outcome.Disposition == domain.AssessmentIncomplete) {
		return true
	}
	if snapshot.SummaryFound {
		state := snapshot.Summary
		return state.Status == marker.StateFailed || state.Status == marker.StateReviewing || len(state.Pending) > 0 || state.LastReviewed != snapshot.PullRequest.Head
	}
	return false
}

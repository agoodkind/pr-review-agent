package reassessment_test

import (
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reassessment"
)

func livePending() reassessment.LiveSnapshot {
	return reassessment.LiveSnapshot{
		PullRequest: githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: "open", Head: currentHead, Title: "Current purpose"},
		CheckFound:  true, Check: githubapp.CheckRun{ID: 71, Status: "completed", Conclusion: "success", Outcome: domain.AssessmentOutcome{Disposition: domain.AssessmentIncomplete, Head: currentHead}},
		SummaryFound: true, Summary: marker.State{Status: marker.StateReviewing, Pending: []string{"unread-chunk"}},
	}
}

func TestTrackingRepairRequiresConfirmedLiveStateAndPreservesFailedReads(t *testing.T) {
	now := time.Now()
	coordinator := planner()
	var target reassessment.Target
	target.Key = "tracked"
	target.Version = 1
	snapshot := livePending()
	first := coordinator.Repair(target, nil, snapshot, now)
	if first.Record != nil || first.RepairKind != "confirming" {
		t.Fatalf("a single observation guessed a missing assessment: %+v", first)
	}
	second := coordinator.Repair(first.Target, nil, snapshot, now.Add(time.Minute))
	if second.Record == nil || second.RepairKind != "missing_queue" || second.Record.Job.Forced {
		t.Fatalf("confirmed pending coverage did not repair the missing queue: %+v", second)
	}
	snapshot.FailureStage = "check"
	failed := coordinator.Repair(second.Target, second.Record, snapshot, now.Add(2*time.Minute))
	if failed.Record != nil || failed.RepairKind != "deferred" || failed.Target.Confirmations != 0 || failed.ExpectedQueueVersion != second.Record.Version {
		t.Fatalf("a failed read discarded or completed pending work: %+v", failed)
	}
	snapshot.FailureStage = ""
	snapshot.PullRequest.EligibilityKnown = false
	partial := coordinator.Repair(second.Target, second.Record, snapshot, now.Add(3*time.Minute))
	if partial.Record != nil || partial.RepairKind != "deferred" {
		t.Fatalf("partial eligibility invented a terminal state: %+v", partial)
	}
}

func TestConfirmedClosureAndReopeningRepairUseDifferentGenerations(t *testing.T) {
	now := time.Now()
	coordinator := planner()
	record := queued(now)
	var target reassessment.Target
	target.Key = record.Key
	snapshot := livePending()
	snapshot.PullRequest.State = "closed"
	first := coordinator.Repair(target, &record, snapshot, now)
	if first.Record != nil {
		t.Fatal("one closure observation terminated the queue")
	}
	closed := coordinator.Repair(first.Target, &record, snapshot, now.Add(time.Minute))
	if closed.Record == nil || closed.Record.Phase != "terminal" || closed.RepairKind != "closed" {
		t.Fatalf("confirmed closure did not stop model work: %+v", closed)
	}
	snapshot = livePending()
	opened := coordinator.Repair(closed.Target, closed.Record, snapshot, now.Add(2*time.Minute))
	if opened.Record != nil {
		t.Fatal("one reopened observation guessed the new lifecycle")
	}
	resumed := coordinator.Repair(opened.Target, closed.Record, snapshot, now.Add(3*time.Minute))
	if resumed.Record == nil || resumed.RepairKind != "reactivated" || resumed.Record.Generation == closed.Record.Generation {
		t.Fatalf("live reopening did not restart a protected generation: %+v", resumed)
	}
}

func TestLostPrivateCompletionRebuildsAssessmentAndRequiresNewReceipt(t *testing.T) {
	now := time.Now()
	coordinator := planner()
	var target reassessment.Target
	target.Key = "tracked"
	snapshot := livePending()
	snapshot.Summary.Status = marker.StateDone
	snapshot.Summary.LastReviewed = currentHead
	snapshot.Summary.Pending = nil
	snapshot.Check.Outcome.Disposition = domain.AssessmentCompleted
	snapshot.Check.Outcome.MetadataRevision = diff.PullRequestRevision(snapshot.PullRequest)
	first := coordinator.Repair(target, nil, snapshot, now)
	second := coordinator.Repair(first.Target, nil, snapshot, now.Add(time.Minute))
	rebuild := coordinator.Repair(second.Target, nil, snapshot, now.Add(2*time.Minute))
	if rebuild.Record == nil || rebuild.RepairKind != "evidence_rebuild" || !rebuild.Record.RebuildEvidence || rebuild.Record.Phase == "terminal" || rebuild.Target.PrivateNonce != "" {
		t.Fatalf("missing private evidence was adopted or stalled: %+v", rebuild)
	}
	continuation := coordinator.Repair(rebuild.Target, rebuild.Record, snapshot, now.Add(3*time.Minute))
	if continuation.Record != nil || continuation.RepairKind != "consistent_pending" {
		t.Fatalf("evidence rebuild never became dispatchable: %+v", continuation)
	}
	validated := reassessment.Snapshot{PullRequest: snapshot.PullRequest}
	dispatched := coordinator.Decide(*rebuild.Record, validated, now.Add(3*time.Minute))
	if !dispatched.Dispatch || dispatched.Job.Forced || !dispatched.Job.AutomaticReassessment {
		t.Fatalf("rebuild did not resume existing checkpoints: %+v", dispatched)
	}
	receipt := domain.AssessmentOutcome{Nonce: "new-private-receipt", Disposition: domain.AssessmentCompleted, Head: currentHead, MetadataRevision: diff.PullRequestRevision(snapshot.PullRequest), CheckRunID: 81, CoverageComplete: true}
	snapshot.Check.ID = 81
	snapshot.Check.Outcome = receipt
	rebuiltTarget := continuation.Target
	rebuiltTarget.PrivateOutcome = receipt
	rebuiltTarget.PrivateNonce = receipt.Nonce
	observed := coordinator.Repair(rebuiltTarget, &dispatched, snapshot, now.Add(4*time.Minute))
	complete := coordinator.Repair(observed.Target, &dispatched, snapshot, now.Add(5*time.Minute))
	if complete.Record == nil || complete.Record.Reason != "completed" {
		t.Fatalf("matching new private/live receipt did not complete reassessment: %+v", complete)
	}
	snapshot.Check.Outcome.Nonce = "different-receipt"
	disagreed := coordinator.Repair(rebuiltTarget, &dispatched, snapshot, now.Add(6*time.Minute))
	if disagreed.Record != nil && disagreed.Record.Reason == "completed" {
		t.Fatal("contradictory live receipt fabricated completion")
	}
	stopped := *rebuild.Record
	stopped.Phase = "terminal"
	stopped.Reason = "draft"
	ready := livePending()
	firstReady := coordinator.Repair(rebuild.Target, &stopped, ready, now.Add(7*time.Minute))
	secondReady := coordinator.Repair(firstReady.Target, &stopped, ready, now.Add(8*time.Minute))
	if secondReady.Record == nil || secondReady.RepairKind != "reactivated" || secondReady.Record.Generation == stopped.Generation {
		t.Fatalf("terminal evidence rebuild prevented live reopening recovery: %+v", secondReady)
	}
}

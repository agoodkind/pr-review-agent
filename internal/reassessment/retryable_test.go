package reassessment_test

import (
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reassessment"
)

func TestPrivatePanicStopsRecoveryForSameAttempt(t *testing.T) {
	now := time.Now()
	coordinator := planner()
	snapshot := livePending()
	snapshot.CheckFound = false
	record := queued(now)
	record.Job.DeliveryID = "panic-delivery"
	record.Generation = "generation"
	record.MetadataRevision = diff.PullRequestRevision(snapshot.PullRequest)
	outcome := domain.AssessmentOutcome{
		Nonce:            "receipt",
		Disposition:      domain.AssessmentFailed,
		Head:             snapshot.PullRequest.Head,
		MetadataRevision: record.MetadataRevision,
		FailureClasses:   []string{"panic"},
	}
	outcome.SetRetryable(false)
	target := reassessment.Target{
		Key:               "tracked",
		PrivateOutcome:    outcome,
		PrivateNonce:      outcome.Nonce,
		PrivateDeliveryID: record.Job.DeliveryID,
		PrivateGeneration: record.Generation,
	}
	first := coordinator.Repair(target, &record, snapshot, now)
	result := coordinator.Repair(first.Target, &record, snapshot, now.Add(time.Minute))
	if result.Record == nil || result.Record.Phase != "terminal" || result.Record.Dispatch {
		t.Fatalf("panic was rescheduled: %+v", result)
	}

	newer := record
	newer.Job.DeliveryID = "explicit-retry"
	newer.Generation = "new-generation"
	result = coordinator.Repair(first.Target, &newer, snapshot, now.Add(time.Minute))
	if result.Record != nil && result.Record.Phase == "terminal" {
		t.Fatalf("earlier panic stopped a new generation: %+v", result)
	}

	snapshot.PullRequest.Title = "Changed purpose"
	result = coordinator.Repair(first.Target, &record, snapshot, now.Add(time.Minute))
	if result.Record != nil && result.Record.Phase == "terminal" {
		t.Fatalf("earlier panic stopped changed metadata: %+v", result)
	}
}

func TestConfirmingPanicDoesNotDispatchWithoutCheck(t *testing.T) {
	now := time.Now()
	snapshot := livePending()
	record := queued(now)
	record.Phase = "confirming"
	record.MetadataRevision = diff.PullRequestRevision(snapshot.PullRequest)
	record.Outcome = domain.AssessmentOutcome{
		Disposition:      domain.AssessmentFailed,
		Head:             record.Head,
		MetadataRevision: record.MetadataRevision,
		FailureClasses:   []string{"panic"},
	}
	record.Outcome.SetRetryable(false)
	result := planner().Decide(
		record,
		reassessment.Snapshot{PullRequest: snapshot.PullRequest},
		now,
	)
	if result.Dispatch || result.Phase != "terminal" {
		t.Fatalf("confirming panic dispatched without a check: %+v", result)
	}

	record.Outcome.Disposition = domain.AssessmentCompleted
	result = planner().Decide(record, reassessment.Snapshot{PullRequest: snapshot.PullRequest}, now)
	if result.Phase == "terminal" {
		t.Fatalf("completion did not wait for check confirmation: %+v", result)
	}
}

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

func TestCurrentPanicCheckPreventsMissingQueueRecovery(t *testing.T) {
	now := time.Now()
	coordinator := planner()
	snapshot := livePending()
	snapshot.Check.Head = snapshot.PullRequest.Head
	snapshot.Check.ExternalID = "panic-delivery"
	snapshot.Check.Outcome = domain.AssessmentOutcome{
		Nonce:            "panic-receipt",
		Disposition:      domain.AssessmentFailed,
		Head:             snapshot.PullRequest.Head,
		MetadataRevision: diff.PullRequestRevision(snapshot.PullRequest),
		FailureClasses:   []string{"panic"},
		CheckRunID:       snapshot.Check.ID,
	}
	snapshot.Check.Outcome.SetRetryable(false)
	target := reassessment.Target{Key: "tracked"}
	first := coordinator.Repair(target, nil, snapshot, now)
	if first.Record != nil || first.RepairKind != "confirming" {
		t.Fatalf("first observation did not wait for confirmation: %+v", first)
	}

	second := coordinator.Repair(first.Target, nil, snapshot, now.Add(time.Minute))
	if second.Record != nil || second.RepairKind != "not_retryable" {
		t.Fatalf("confirmed panic created a retry: %+v", second)
	}

	older := target
	older.PrivateNonce = "previous-receipt"
	older.PrivateDeliveryID = "previous-delivery"
	older.PrivateOutcome = snapshot.Check.Outcome
	older.PrivateOutcome.CheckRunID--
	older.PrivateOutcome.Nonce = older.PrivateNonce
	older.PrivateOutcome.Disposition = domain.AssessmentCompleted
	first = coordinator.Repair(older, nil, snapshot, now)
	second = coordinator.Repair(first.Target, nil, snapshot, now.Add(time.Minute))
	if second.Record != nil || second.RepairKind != "not_retryable" {
		t.Fatalf("older private outcome replaced the current check: %+v", second)
	}

	newerPrivate := older
	newerPrivate.PrivateOutcome.CheckRunID = snapshot.Check.ID + 1
	newerPrivate.PrivateOutcome.Disposition = domain.AssessmentFailed
	newerPrivate.PrivateOutcome.FailureClasses = []string{"other"}
	newerPrivate.PrivateOutcome.SetRetryable(false)
	first = coordinator.Repair(newerPrivate, nil, snapshot, now)
	second = coordinator.Repair(first.Target, nil, snapshot, now.Add(time.Minute))
	if second.Record == nil || second.RepairKind != "missing_queue" {
		t.Fatalf("older check suppressed newer private work: %+v", second)
	}

	for _, scenario := range []string{"head", "check head", "revision", "legacy", "check ID"} {
		t.Run(
			scenario,
			func(t *testing.T) {
				changed := snapshot
				switch scenario {
				case "head":
					changed.Check.Outcome.Head = domain.HeadSHA("older-head")
				case "check head":
					changed.Check.Head = domain.HeadSHA("older-head")
				case "revision":
					changed.Check.Outcome.MetadataRevision = "older-revision"
				case "legacy":
					changed.Check.Outcome.Retryable = nil
				case "check ID":
					changed.Check.Outcome.CheckRunID++
				}

				initial := coordinator.Repair(target, nil, changed, now)
				repaired := coordinator.Repair(initial.Target, nil, changed, now.Add(time.Minute))
				if repaired.Record == nil || repaired.RepairKind != "missing_queue" {
					t.Fatalf("stale or legacy evidence prevented recovery: %+v", repaired)
				}
			},
		)
	}

	newer := queued(now)
	newer.Job.DeliveryID = "explicit-retry"
	newer.Generation = "new-generation"
	newer.MetadataRevision = snapshot.Check.Outcome.MetadataRevision
	first = coordinator.Repair(target, &newer, snapshot, now)
	second = coordinator.Repair(first.Target, &newer, snapshot, now.Add(time.Minute))
	if second.Record != nil || second.RepairKind != "consistent_pending" {
		t.Fatalf("earlier check stopped newer work: %+v", second)
	}
}

func TestCurrentPanicCheckPreservesMatchingTerminalAttempt(t *testing.T) {
	now := time.Now()
	coordinator := planner()
	snapshot := livePending()
	snapshot.Check.Head = snapshot.PullRequest.Head
	snapshot.Check.ExternalID = "panic-delivery"
	snapshot.Check.Outcome = domain.AssessmentOutcome{
		Nonce:            "panic-receipt",
		Disposition:      domain.AssessmentFailed,
		Head:             snapshot.PullRequest.Head,
		MetadataRevision: diff.PullRequestRevision(snapshot.PullRequest),
		CheckRunID:       snapshot.Check.ID,
		FailureClasses:   []string{"panic"},
	}
	snapshot.Check.Outcome.SetRetryable(false)
	record := queued(now)
	record.Phase = "terminal"
	record.Job.DeliveryID = snapshot.Check.ExternalID
	record.Head = snapshot.Check.Head
	record.MetadataRevision = snapshot.Check.Outcome.MetadataRevision
	record.Outcome = snapshot.Check.Outcome
	target := reassessment.Target{Key: record.Key}
	first := coordinator.Repair(target, &record, snapshot, now)
	second := coordinator.Repair(first.Target, &record, snapshot, now.Add(time.Minute))
	if second.Record != nil || second.RepairKind != "not_retryable" {
		t.Fatalf("matching terminal attempt was reactivated: %+v", second)
	}

	for _, phase := range []string{"waiting", "running"} {
		newer := record
		newer.Phase = phase
		newer.Generation = "new-generation"
		newer.Job.DeliveryID = "explicit-retry"
		first = coordinator.Repair(target, &newer, snapshot, now)
		second = coordinator.Repair(first.Target, &newer, snapshot, now.Add(time.Minute))
		if second.Record != nil || second.RepairKind != "consistent_pending" {
			t.Fatalf("earlier check stopped %s work: %+v", phase, second)
		}
	}

	different := record
	different.Outcome.Nonce = "different-receipt"
	first = coordinator.Repair(target, &different, snapshot, now)
	second = coordinator.Repair(first.Target, &different, snapshot, now.Add(time.Minute))
	if second.Record == nil || second.RepairKind != "reactivated" {
		t.Fatalf("unmatched terminal attempt prevented recovery: %+v", second)
	}

	different = record
	different.Job.CheckRunID = snapshot.Check.ID + 1
	first = coordinator.Repair(target, &different, snapshot, now)
	second = coordinator.Repair(first.Target, &different, snapshot, now.Add(time.Minute))
	if second.Record == nil || second.RepairKind != "reactivated" {
		t.Fatalf("unmatched job check prevented recovery: %+v", second)
	}

	different = record
	different.Job.AutomaticReassessment = true
	different.AttemptID = "different-attempt"
	first = coordinator.Repair(target, &different, snapshot, now)
	second = coordinator.Repair(first.Target, &different, snapshot, now.Add(time.Minute))
	if second.Record == nil || second.RepairKind != "reactivated" {
		t.Fatalf("unmatched automatic attempt prevented recovery: %+v", second)
	}
}

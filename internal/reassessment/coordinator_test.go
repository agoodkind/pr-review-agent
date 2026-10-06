package reassessment_test

import (
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/reassessment"
)

const (
	currentHead domain.HeadSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newerHead   domain.HeadSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func planner() *reassessment.Coordinator {
	return &reassessment.Coordinator{Settings: config.Reassessment{Enabled: true, InitialDelay: "5m", MaximumDelay: "1h", TTL: "0s", TerminalRetention: "168h", ReconcileInterval: "15m", ConfirmationInterval: "1m", StateConfirmations: 2, ReadBudget: 3, BatchSize: 10, PageSize: 100}}
}

func queued(now time.Time) reassessment.Record {
	return reassessment.Record{ID: "assessment", Generation: "generation", Phase: "waiting", Head: currentHead, CreatedAtMS: now.Add(-72 * time.Hour).UnixMilli(), Job: domain.ReviewJob{PullRequestRef: domain.PullRequestRef{Head: currentHead}}}
}

func TestOnlyOpenUnmergedReadyPullRequestsDispatch(t *testing.T) {
	now := time.Now()
	for _, item := range []struct {
		name   string
		state  string
		merged bool
		draft  bool
	}{
		{name: "closed", state: "closed"}, {name: "merged", state: "closed", merged: true}, {name: "draft", state: "open", draft: true},
	} {
		t.Run(item.name, func(t *testing.T) {
			result := planner().Decide(queued(now), reassessment.Snapshot{PullRequest: githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: item.state, Merged: item.merged, Draft: item.draft, Head: currentHead}}, now)
			if result.Dispatch || result.Phase != "terminal" || result.Reason != item.name {
				t.Fatalf("closed assessment dispatched or canceled incorrectly: %+v", result)
			}
		})
	}
	result := planner().Decide(queued(now), reassessment.Snapshot{PullRequest: githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: "open", Head: currentHead}}, now)
	if !result.Dispatch || result.Job.Forced || !result.Job.AutomaticReassessment || result.Job.Head != currentHead {
		t.Fatalf("ready assessment did not resume normally: %+v", result)
	}
}

func TestQuotaDeferralStartsNoAttemptAndUnlimitedPolicyDoesNotExpire(t *testing.T) {
	now := time.Now()
	record := queued(now)
	record.Attempts = 1000
	available := now.Add(24 * time.Hour)
	result := planner().Decide(record, reassessment.Snapshot{PullRequest: githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: "open", Head: currentHead}, AvailableAt: available}, now)
	if result.Dispatch || result.Attempts != 1000 || result.AttemptID != "" || result.Reason != "quota_wait" || result.NotBeforeMS < available.UnixMilli() {
		t.Fatalf("quota deferral consumed work or expired: %+v", result)
	}
	result = planner().Decide(result, reassessment.Snapshot{PullRequest: githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: "open", Head: currentHead}}, available.Add(time.Minute))
	if !result.Dispatch || result.Phase != "running" {
		t.Fatalf("unlimited policy abandoned an eligible assessment: %+v", result)
	}
}

func TestSuccessfulIncompleteCheckDoesNotCompleteAssessment(t *testing.T) {
	now := time.Now()
	snapshot := reassessment.Snapshot{PullRequest: githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: "open", Head: currentHead}, LatestFound: true, LatestCheck: githubapp.CheckRun{Status: "completed", Conclusion: "success", Outcome: domain.AssessmentOutcome{Disposition: domain.AssessmentIncomplete, Head: currentHead}}}
	result := planner().Decide(queued(now), snapshot, now)
	if !result.Dispatch {
		t.Fatalf("successful incomplete check suppressed retry: %+v", result)
	}
	snapshot.LatestCheck.Outcome.Disposition = domain.AssessmentCompleted
	snapshot.LatestCheck.Outcome.CoverageComplete = true
	snapshot.LatestCheck.Outcome.MetadataRevision = diff.PullRequestRevision(snapshot.PullRequest)
	result = planner().Decide(queued(now), snapshot, now)
	if result.Dispatch || result.Phase != "terminal" || result.Reason != "completed" {
		t.Fatalf("completed assessment did not terminate retry: %+v", result)
	}
}

func TestMetadataEditCannotReuseEarlierCompletion(t *testing.T) {
	now := time.Now()
	pr := githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: "open", Head: currentHead, Title: "Earlier purpose"}
	snapshot := reassessment.Snapshot{PullRequest: pr, LatestFound: true, LatestCheck: githubapp.CheckRun{Status: "completed", Outcome: domain.AssessmentOutcome{Disposition: domain.AssessmentCompleted, Head: currentHead, MetadataRevision: diff.PullRequestRevision(pr)}}}
	snapshot.PullRequest.Title = "Changed purpose"
	result := planner().Decide(queued(now), snapshot, now)
	if !result.Dispatch || result.Phase == "terminal" {
		t.Fatalf("changed metadata reused an earlier assessment: %+v", result)
	}
}

func TestNewHeadReplacesQueuedTargetAndRunningAttemptResumesOnce(t *testing.T) {
	now := time.Now()
	record := queued(now)
	record.Phase = "running"
	record.AttemptID = "existing-attempt"
	record.Attempts = 3
	record.Job.DeliveryID = record.AttemptID
	result := planner().Decide(record, reassessment.Snapshot{PullRequest: githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: "open", Head: currentHead}}, now)
	if !result.Dispatch || result.AttemptID != "existing-attempt" || result.Attempts != 3 {
		t.Fatalf("resume allocated another attempt: %+v", result)
	}
	result = planner().Decide(record, reassessment.Snapshot{PullRequest: githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: "open", Head: newerHead}}, now)
	if !result.Dispatch || result.Head != newerHead || result.Generation == record.Generation || result.AttemptID == record.AttemptID || result.Job.Forced {
		t.Fatalf("new head reused stale work: %+v", result)
	}
}

func TestDeclinedAssessmentIsNotReportedAsCompleted(t *testing.T) {
	now := time.Now()
	record := queued(now)
	record.Phase = "confirming"
	result := planner().Decide(record, reassessment.Snapshot{PullRequest: githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: "open", Head: currentHead}, AttemptFound: true, AttemptCheck: githubapp.CheckRun{Status: "completed", Conclusion: "action_required", Outcome: domain.AssessmentOutcome{Disposition: domain.AssessmentDeclined, Head: currentHead}}}, now)
	if result.Dispatch || result.Reason != "declined" {
		t.Fatalf("declined work counted as reviewed: %+v", result)
	}
}

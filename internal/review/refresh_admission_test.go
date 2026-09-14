package review_test

import (
	"context"
	"errors"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
)

// Two quick discussion events can enter the keyed review queue before either
// refresh runs. Each accepted event needs its own pending check, so the first
// refresh cannot make the second look finished while it is still waiting.
func TestEachReviewRefreshOwnsAPendingCheckUntilItsReconciliationFinishes(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})
	fixture.state.checkRuns = append(fixture.state.checkRuns, map[string]any{
		"id":          float64(4242),
		"name":        config.ReviewCheckName,
		"head_sha":    testHeadSHA,
		"status":      "completed",
		"conclusion":  "success",
		"external_id": "",
	})
	firstJob := fixture.job()
	firstJob.DeliveryID = "refresh-one"
	firstJob.RefreshVerdict = true

	first, wasAdmitted, err := fixture.service.Admit(context.Background(), firstJob)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !wasAdmitted {
		t.Fatal("the review refresh was not admitted")
	}
	if first.CheckRunStatus != "in_progress" || first.CheckRunConclusion != "" {
		t.Fatalf("check = %q/%q, want in_progress with no conclusion",
			first.CheckRunStatus, first.CheckRunConclusion)
	}

	secondJob := fixture.job()
	secondJob.DeliveryID = "refresh-two"
	secondJob.RefreshVerdict = true
	second, secondAdmitted, err := fixture.service.Admit(context.Background(), secondJob)
	if err != nil {
		t.Fatalf("second Admit: %v", err)
	}
	if !secondAdmitted {
		t.Fatal("the second review refresh was not admitted")
	}
	if first.CheckRunID == second.CheckRunID || len(fixture.state.checkRuns) != 3 {
		t.Fatalf("refresh check ids/count = %d/%d/%d, want one check per accepted refresh",
			first.CheckRunID, second.CheckRunID, len(fixture.state.checkRuns))
	}
	if second.CheckRunStatus != "in_progress" || second.CheckRunConclusion != "" {
		t.Fatalf("second check = %q/%q, want in_progress with no conclusion",
			second.CheckRunStatus, second.CheckRunConclusion)
	}

	for _, checkRun := range fixture.state.checkRuns {
		checkRunID, _ := checkRun["id"].(float64)
		if int64(checkRunID) != first.CheckRunID {
			continue
		}
		checkRun["status"] = "completed"
		checkRun["conclusion"] = "success"
	}
	_, redeliveryAdmitted, err := fixture.service.Admit(context.Background(), firstJob)
	if err != nil {
		t.Fatalf("redelivery Admit: %v", err)
	}
	if redeliveryAdmitted {
		t.Fatal("the completed refresh delivery was admitted twice")
	}
	if len(fixture.state.checkRuns) != 3 {
		t.Fatalf("check runs after redelivery = %d, want 3", len(fixture.state.checkRuns))
	}
}

// Admission starts the visible check before the queue accepts the job. If the
// queue refuses it, the same delivery must be able to resume that check after
// capacity returns. A terminal failed check cannot mean the work finished.
func TestARejectedReviewRefreshCanResumeOnRedelivery(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})
	job := fixture.job()
	job.DeliveryID = "refresh-rejected"
	job.RefreshVerdict = true

	admitted, wasAdmitted, err := fixture.service.Admit(context.Background(), job)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !wasAdmitted {
		t.Fatal("the review refresh was not admitted")
	}
	queueErr := errors.New("review queue is full")
	if err := fixture.service.Reject(context.Background(), admitted, queueErr); !errors.Is(err, queueErr) {
		t.Fatalf("Reject: %v, want the queue failure", err)
	}
	if conclusion := fixture.state.lastUpdateCheckRun["conclusion"]; conclusion != "cancelled" {
		t.Fatalf("rejected check conclusion = %v, want cancelled", conclusion)
	}

	resumed, retryAdmitted, err := fixture.service.Admit(context.Background(), job)
	if err != nil {
		t.Fatalf("redelivery Admit: %v", err)
	}
	if !retryAdmitted {
		t.Fatal("the redelivery was suppressed after no background work accepted it")
	}
	if resumed.CheckRunID != admitted.CheckRunID {
		t.Fatalf("resumed check = %d, want the original %d", resumed.CheckRunID, admitted.CheckRunID)
	}
	if resumed.CheckRunStatus != "in_progress" || resumed.CheckRunConclusion != "" {
		t.Fatalf("resumed check = %q/%q, want in_progress with no conclusion",
			resumed.CheckRunStatus, resumed.CheckRunConclusion)
	}
}

// A retry of an older delivery must become the newest visible check. Reusing
// its hidden cancelled check would leave a newer success standing while the
// retry runs. A second rejection must then find this newest retry check.
func TestAnOlderReviewRefreshRetryCreatesANewVisibleCheck(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})
	olderJob := fixture.job()
	olderJob.DeliveryID = "refresh-older"
	olderJob.RefreshVerdict = true

	older, _, err := fixture.service.Admit(context.Background(), olderJob)
	if err != nil {
		t.Fatalf("older Admit: %v", err)
	}
	queueErr := errors.New("review queue is full")
	if err := fixture.service.Reject(context.Background(), older, queueErr); !errors.Is(err, queueErr) {
		t.Fatalf("older Reject: %v, want the queue failure", err)
	}

	newerJob := fixture.job()
	newerJob.DeliveryID = "refresh-newer"
	newerJob.RefreshVerdict = true
	newer, _, err := fixture.service.Admit(context.Background(), newerJob)
	if err != nil {
		t.Fatalf("newer Admit: %v", err)
	}
	for _, checkRun := range fixture.state.checkRuns {
		checkRunID, _ := checkRun["id"].(float64)
		if int64(checkRunID) != newer.CheckRunID {
			continue
		}
		checkRun["status"] = "completed"
		checkRun["conclusion"] = "success"
	}

	retry, retryAdmitted, err := fixture.service.Admit(context.Background(), olderJob)
	if err != nil {
		t.Fatalf("retry Admit: %v", err)
	}
	if !retryAdmitted {
		t.Fatal("the older delivery retry was suppressed")
	}
	if retry.CheckRunID == older.CheckRunID || retry.CheckRunID == newer.CheckRunID {
		t.Fatalf("retry check = %d, want a new check after older %d and newer %d",
			retry.CheckRunID, older.CheckRunID, newer.CheckRunID)
	}
	if err := fixture.service.Reject(context.Background(), retry, queueErr); !errors.Is(err, queueErr) {
		t.Fatalf("retry Reject: %v, want the queue failure", err)
	}

	resumed, resumedAdmitted, err := fixture.service.Admit(context.Background(), olderJob)
	if err != nil {
		t.Fatalf("second retry Admit: %v", err)
	}
	if !resumedAdmitted {
		t.Fatal("the second retry was suppressed")
	}
	if resumed.CheckRunID != retry.CheckRunID {
		t.Fatalf("second retry check = %d, want newest retry check %d",
			resumed.CheckRunID, retry.CheckRunID)
	}
	if resumed.CheckRunStatus != "in_progress" || resumed.CheckRunConclusion != "" {
		t.Fatalf("second retry check = %q/%q, want in_progress with no conclusion",
			resumed.CheckRunStatus, resumed.CheckRunConclusion)
	}
}

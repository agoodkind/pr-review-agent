package reassessment_test

import (
	"fmt"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/reassessment"
	"goodkind.io/pr-review-agent/internal/webhook"
)

func TestLifecycleWebhookReactivatesCanceledPendingAssessment(t *testing.T) {
	now := time.Now()
	for _, item := range []struct {
		action string
		state  string
		draft  bool
	}{
		{action: "reopened", state: "closed"},
		{action: "ready_for_review", state: "open", draft: true},
	} {
		t.Run(item.action, func(t *testing.T) {
			coordinator := planner()
			prior := coordinator.Decide(queued(now), reassessment.Snapshot{PullRequest: githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: item.state, Draft: item.draft, Head: currentHead}}, now)
			payload := []byte(fmt.Sprintf(`{"action":%q,"installation":{"id":42},"repository":{"name":"repo","owner":{"login":"owner"}},"pull_request":{"number":7,"draft":false,"head":{"sha":%q}}}`, item.action, string(currentHead)))
			event, supported, err := webhook.ParseEvent("pull_request", "delivery-reactivated", payload)
			if err != nil || !supported {
				t.Fatalf("lifecycle webhook was not admitted: event=%+v err=%v", event, err)
			}
			fresh := githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: "open", Head: currentHead, Title: "Resume the assessment"}
			if !event.RestartsQueue() {
				t.Fatal("lifecycle webhook did not request queue evaluation")
			}
			restarted, changed := coordinator.Reactivate(&prior, event.Job(), fresh, now)
			if !changed || restarted.Phase != "waiting" || restarted.Generation == prior.Generation || restarted.Job.Forced || restarted.Head != currentHead || restarted.MetadataRevision != diff.PullRequestRevision(fresh) {
				t.Fatalf("eligible lifecycle did not restart current evaluation: %+v", restarted)
			}
			again, changed := coordinator.Reactivate(&restarted, event.Job(), fresh, now.Add(time.Second))
			if changed || again.Generation != restarted.Generation {
				t.Fatalf("duplicate lifecycle allocated another generation: %+v", again)
			}
			staleJob := prior.Job
			staleJob.AutomaticReassessment = true
			staleJob.ReassessmentGeneration = prior.Generation
			if restarted.AcceptsOutcome(staleJob) {
				t.Fatal("an earlier lifecycle outcome can replace the reactivated assessment")
			}
			result := coordinator.Decide(restarted, reassessment.Snapshot{PullRequest: fresh}, now)
			if !result.Dispatch || !result.Job.AutomaticReassessment || result.Job.Forced {
				t.Fatalf("reactivated assessment did not resume existing checkpoints: %+v", result)
			}
		})
	}
}

func TestReactivationRecoversExpiredTrackingAndReusesCurrentCompletion(t *testing.T) {
	now := time.Now()
	coordinator := planner()
	fresh := githubapp.PullRequest{EligibilityKnown: true, MetadataKnown: true, State: "open", Head: currentHead, Title: "Current purpose"}
	job := domain.ReviewJob{DeliveryID: "delivery-reopened", PullRequestRef: domain.PullRequestRef{Repository: domain.Repository{Owner: "owner", Name: "repo"}, InstallationID: 42, Number: 7, Head: currentHead}}
	restarted, changed := coordinator.Reactivate(nil, job, fresh, now)
	if !changed || restarted.Phase != "waiting" {
		t.Fatalf("expired tracking did not restart evaluation: %+v", restarted)
	}
	check := githubapp.CheckRun{Status: "completed", Outcome: domain.AssessmentOutcome{Disposition: domain.AssessmentCompleted, Head: currentHead, MetadataRevision: diff.PullRequestRevision(fresh)}}
	result := coordinator.Decide(restarted, reassessment.Snapshot{PullRequest: fresh, LatestCheck: check, LatestFound: true}, now)
	if result.Dispatch || result.Reason != "completed" {
		t.Fatalf("current completion unnecessarily restarted model work: %+v", result)
	}
	fresh.Draft = true
	if _, changed := coordinator.Reactivate(nil, job, fresh, now); changed {
		t.Fatal("draft status restarted assessment")
	}
}

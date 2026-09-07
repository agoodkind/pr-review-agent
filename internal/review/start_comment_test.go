package review_test

import (
	"context"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

// The start comment names the active run without moving its prior checkpoint.
func TestTheStartCommentCarriesTheCurrentRunAndPreservesTheBaseline(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})
	seedReviewedBaseline(fixture)
	job := fixture.job()

	if err := fixture.run(context.Background(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fixture.state.issueCommentBodies) == 0 {
		t.Fatal("the pull request was never told the review started")
	}

	state, found := marker.DecodeState(fixture.state.issueCommentBodies[0])
	if !found {
		t.Fatalf("the start comment has no durable state: %q", fixture.state.issueCommentBodies[0])
	}
	if state.RunID != job.DeliveryID || state.Status != marker.StateReviewing {
		t.Fatalf("start state = run %q status %q, want run %q status %q",
			state.RunID, state.Status, job.DeliveryID, marker.StateReviewing)
	}
	if state.LastReviewed != domain.HeadSHA(coveragePriorHead) {
		t.Fatalf("start baseline = %q, want %q", state.LastReviewed, coveragePriorHead)
	}
}

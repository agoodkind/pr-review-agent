package review_test

import (
	"context"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

func TestInlineVerdictFinalThreadResolutionWithdrawsOnlyOwnUnsupportedBlock(t *testing.T) {
	thread := botThreadAt("resolved-during-report", 5, false)
	model := &resolveInlineDuringReportModel{sequenceModel: &sequenceModel{results: []domain.ReviewResult{{}}}}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model:            model,
		reconcileThreads: []githubapp.ReviewThread{thread},
		reviewPages: [][]map[string]any{{
			{
				"id": float64(31), "commit_id": coveragePriorHead, "state": "CHANGES_REQUESTED",
				"body": marker.Review(domain.HeadSHA(coveragePriorHead), domain.ReviewDecisionRequestChanges),
				"user": map[string]any{"login": testBotLogin},
			},
			{
				"id": float64(32), "commit_id": coveragePriorHead, "state": "APPROVED",
				"body": marker.Review(domain.HeadSHA(coveragePriorHead), domain.ReviewDecisionApprove),
				"user": map[string]any{"login": testBotLogin},
			},
			{
				"id": float64(33), "commit_id": coveragePriorHead, "state": "CHANGES_REQUESTED",
				"body": "A human requires another change.",
				"user": map[string]any{"login": "human-reviewer"},
			},
		}},
	})
	model.state = fixture.state
	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run succeeded after the last actionable thread disappeared")
	}
	if fixture.state.lastSubmitReview != nil {
		t.Fatalf("submitted review = %v, want no guessed verdict", fixture.state.lastSubmitReview)
	}
	wantStates := []string{"DISMISSED", "APPROVED", "CHANGES_REQUESTED"}
	for index, want := range wantStates {
		if got := fixture.state.reviewPages[0][index]["state"]; got != want {
			t.Fatalf("review %d state = %v, want %s", index, got, want)
		}
	}
}

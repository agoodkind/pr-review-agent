package review_test

import (
	"context"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestInlineVerdictWithheldSummaryRequiresOwnAuthorAndExactHead(t *testing.T) {
	cases := []struct {
		name   string
		author string
		head   domain.HeadSHA
	}{
		{name: "another head", author: testBotLogin, head: domain.HeadSHA(testStaleHeadSHA)},
		{name: "another author", author: "human-reviewer", head: domain.HeadSHA(testHeadSHA)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newServiceFixture(t, serviceFixtureOptions{
				reviewPages: blockingVerdictReviewPage(domain.HeadSHA(testHeadSHA), true),
			})
			body := review.RenderBody(review.Summary{
				Head: testCase.head, Decision: domain.ReviewDecisionComment, ApprovalWithheld: true,
			}) + "\n" + marker.EncodeState(marker.State{
				LastReviewed: domain.HeadSHA(testHeadSHA), RunID: "earlier-delivery", Status: marker.StateDone,
			})
			fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
				"id": float64(2100), "body": body, "user": map[string]any{"login": testCase.author},
			})

			if err := fixture.run(context.Background(), fixture.job()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
				t.Fatalf("event = %v, want APPROVE despite unrelated withheld summary", fixture.state.lastSubmitReview["event"])
			}
		})
	}
}

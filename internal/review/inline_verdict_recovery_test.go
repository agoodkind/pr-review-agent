package review_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
)

func TestInlineVerdictDismissalFailureCannotApproveOnRetry(t *testing.T) {
	model := &sequenceModel{results: []domain.ReviewResult{{Findings: []domain.Finding{severeFinding()}}}}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: model, minimumImportance: 9, createCommentStatus: http.StatusUnprocessableEntity,
		reviewPages: blockingVerdictReviewPage(domain.HeadSHA(testHeadSHA), true),
	})
	fixture.state.dismissReviewStatus = http.StatusForbidden
	if err := fixture.run(context.Background(), fixture.forcedJob()); err == nil {
		t.Fatal("Run succeeded although GitHub refused dismissal of the old review")
	}
	if len(fixture.state.dismissals) != 0 || fixture.state.reviewPages[0][0]["state"] != "CHANGES_REQUESTED" {
		t.Fatalf("denied dismissal counted as cleanup: dismissals=%v review=%v", fixture.state.dismissals, fixture.state.reviewPages[0][0])
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf("conclusion = %v, want failed cleanup reported", fixture.state.lastUpdateCheckRun["conclusion"])
	}
	fixture.state.dismissReviewStatus = http.StatusOK
	job := fixture.job()
	job.DeliveryID = "retry-after-dismissal-restored"
	if err := fixture.run(context.Background(), job); err != nil {
		t.Fatalf("retry Run: %v", err)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionComment) {
		t.Fatalf("retry event = %v, want COMMENT while severe finding remains unplaced", fixture.state.lastSubmitReview["event"])
	}
	if fixture.state.reviewPages[0][0]["state"] != "DISMISSED" || len(fixture.state.dismissals) != 1 {
		t.Fatalf("recovered cleanup = %v review=%v, want one successful dismissal", fixture.state.dismissals, fixture.state.reviewPages[0][0])
	}
	if !strings.Contains(failureSummaryComment(t, fixture), "Severe defect") {
		t.Fatal("retry lost the severe finding that GitHub refused to place inline")
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "action_required" {
		t.Fatalf("retry conclusion = %v, want action_required", fixture.state.lastUpdateCheckRun["conclusion"])
	}
}

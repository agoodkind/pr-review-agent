package review_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestInlineVerdictIgnoresThreadsWithoutAnActionableBotFinding(t *testing.T) {
	cases := []struct {
		name  string
		alter func(*githubapp.ReviewThread)
	}{
		{name: "empty body", alter: func(thread *githubapp.ReviewThread) { thread.RootComment.Body = "" }},
		{name: "whitespace body", alter: func(thread *githubapp.ReviewThread) { thread.RootComment.Body = " \n\t" }},
		{name: "empty path", alter: func(thread *githubapp.ReviewThread) { thread.RootComment.Path = "" }},
		{name: "whitespace path", alter: func(thread *githubapp.ReviewThread) { thread.RootComment.Path = " \t" }},
		{name: "no line", alter: func(thread *githubapp.ReviewThread) { thread.RootComment.StartLine = 0; thread.RootComment.EndLine = 0 }},
		{name: "negative line", alter: func(thread *githubapp.ReviewThread) {
			thread.RootComment.StartLine = -1
			thread.RootComment.EndLine = -1
		}},
		{name: "resolved", alter: func(thread *githubapp.ReviewThread) { thread.Resolved = true }},
		{name: "foreign author", alter: func(thread *githubapp.ReviewThread) { thread.RootComment.Author = "another-reviewer" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			thread := botThreadAt("existing-thread", 5, false)
			test.alter(&thread)
			fixture := newServiceFixture(t, serviceFixtureOptions{
				model:            &sequenceModel{results: []domain.ReviewResult{{}}},
				reconcileThreads: []githubapp.ReviewThread{thread},
			})
			if err := fixture.run(context.Background(), fixture.job()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
				t.Fatalf("review event = %v, want APPROVE after a complete clean review without an actionable bot finding", fixture.state.lastSubmitReview["event"])
			}
		})
	}
}

func TestInlineVerdictSummaryFallbackStillBlocksForExistingActionableThread(t *testing.T) {
	thread := botThreadAt("existing-actionable-thread", 5, false)
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance:   9,
		createCommentStatus: http.StatusUnprocessableEntity,
		reconcileThreads:    []githubapp.ReviewThread{thread},
		model:               &sequenceModel{results: []domain.ReviewResult{{Findings: []domain.Finding{severeFinding()}}}},
	})
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("review event = %v, want REQUEST_CHANGES for the existing actionable inline thread", fixture.state.lastSubmitReview["event"])
	}
	if len(fixture.state.streamedComments) != 0 {
		t.Fatalf("posted inline comments = %v, want none after GitHub refused publication", fixture.state.streamedComments)
	}
	body := failureSummaryComment(t, fixture)
	for _, expected := range []string{"GitHub could not place the finding inline", "Severe defect", "`main.go`:5"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("summary omitted %q:\n%s", expected, body)
		}
	}
}

type resolveInlineDuringReportModel struct {
	*sequenceModel
	state       *serviceServerState
	reportCalls int
}

func (model *resolveInlineDuringReportModel) Report(context.Context, string) (review.ReportCompletion, error) {
	model.reportCalls++
	model.state.mu.Lock()
	defer model.state.mu.Unlock()
	for _, thread := range model.state.threadNodes {
		thread["isResolved"] = true
	}
	return review.ReportCompletion{Model: testReviewModel, Report: review.Report{
		Summary:       "The review examined the current change.",
		Walkthrough:   []string{"The changed line preserves the expected behavior."},
		VerdictReason: "The previous inline finding requires action.",
	}}, nil
}

func TestInlineVerdictRechecksThreadsAfterFinalReport(t *testing.T) {
	thread := botThreadAt("resolved-during-report", 5, false)
	model := &resolveInlineDuringReportModel{sequenceModel: &sequenceModel{results: []domain.ReviewResult{{}}}}
	fixture := newServiceFixture(t, serviceFixtureOptions{model: model, reconcileThreads: []githubapp.ReviewThread{thread}})
	model.state = fixture.state
	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run succeeded after the last actionable thread disappeared before publication")
	}
	if model.reportCalls != 1 {
		t.Fatalf("report calls = %d, want the thread resolved during final reporting", model.reportCalls)
	}
	if fixture.state.lastSubmitReview != nil {
		t.Fatalf("submitted review = %v after actionable thread disappeared", fixture.state.lastSubmitReview)
	}
}

func TestInlineVerdictFallbackRemainsCommentOnRepeatedRefresh(t *testing.T) {
	for _, priorState := range []string{"", "APPROVED", "CHANGES_REQUESTED"} {
		t.Run(priorState, func(t *testing.T) {
			model := &sequenceModel{results: []domain.ReviewResult{{Findings: []domain.Finding{severeFinding()}}}}
			fixture := newServiceFixture(t, serviceFixtureOptions{model: model, minimumImportance: 9, createCommentStatus: http.StatusUnprocessableEntity})
			if priorState != "" {
				decision := domain.ReviewDecisionApprove
				if priorState == "CHANGES_REQUESTED" {
					decision = domain.ReviewDecisionRequestChanges
				}
				fixture.state.reviewPages = [][]map[string]any{{{
					"id": float64(31), "commit_id": coveragePriorHead, "state": priorState,
					"body": marker.Review(domain.HeadSHA(coveragePriorHead), decision), "user": map[string]any{"login": testBotLogin},
				}}}
			}
			for _, delivery := range []string{"first-fallback", "refresh-fallback", "refresh-fallback-again"} {
				job := fixture.job()
				job.DeliveryID = delivery
				if err := fixture.run(context.Background(), job); err != nil {
					t.Fatalf("Run %s: %v", delivery, err)
				}
				if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionComment) {
					t.Fatalf("review event on %s = %v, want COMMENT for unplaced finding", delivery, fixture.state.lastSubmitReview["event"])
				}
			}
			if model.callCount != 1 {
				t.Fatalf("analysis calls = %d, want one completed review", model.callCount)
			}
			if priorState != "" && fixture.state.reviewPages[0][0]["state"] != "DISMISSED" {
				t.Fatalf("prior review still stands: %v", fixture.state.reviewPages[0][0])
			}
			if fixture.state.lastUpdateCheckRun["conclusion"] != "action_required" {
				t.Fatalf("conclusion = %v, want action_required for unplaced finding", fixture.state.lastUpdateCheckRun["conclusion"])
			}
			output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
			if !ok {
				t.Fatal("check output is not an object")
			}
			title, _ := output["title"].(string)
			if strings.Contains(strings.ToLower(title), "approved") {
				t.Fatalf("COMMENT check title claims approval: %q", title)
			}
		})
	}
}

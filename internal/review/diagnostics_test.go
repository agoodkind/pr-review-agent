package review_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestFileAssessmentValidationReportsRawResult(t *testing.T) {
	files := classificationGitSources(t, 168, reviewrules.ContentSource)
	input := diff.ReviewInput{Files: files}
	_, _, validationError := review.PlanFileAssessments(input, policytest.Load(t))
	if validationError == nil {
		t.Fatal("The assessment record must exceed its configured byte limit.")
	}

	options := serviceFixtureOptions{reconcileErr: validationError}
	fixture := newServiceFixture(t, options)
	runError := fixture.run(t.Context(), fixture.job())
	if runError == nil {
		t.Fatal("The review must return the validation error.")
	}

	body := failureSummaryComment(t, fixture)
	if !strings.Contains(body, "| `result` | `"+runError.Error()+"` |") ||
		!strings.Contains(body, "Review stopped during file assessment validation.") ||
		strings.Contains(body, "unexpected error") ||
		strings.Contains(body, "during model analysis") {
		t.Fatalf("The comment must report the known validation error: %s", body)
	}
}

func TestRepeatedPreReviewFailuresReplaceThePreviousFailure(t *testing.T) {
	options := serviceFixtureOptions{
		reconcileErr: errors.New("reconciliation failed before review"),
	}
	fixture := newServiceFixture(t, options)
	job := fixture.job()
	job.DeliveryID = "first-failed-attempt"
	if err := fixture.run(t.Context(), job); err == nil {
		t.Fatal("The first review must return the reconciliation failure.")
	}

	job.DeliveryID = "second-failed-attempt"
	if err := fixture.run(t.Context(), job); err == nil {
		t.Fatal("The second review must return the reconciliation failure.")
	}

	body := failureSummaryComment(t, fixture)
	if strings.Count(body, "Review failed while reconciling existing findings.") != 1 ||
		strings.Count(body, "<summary>Review details</summary>") != 1 ||
		strings.Contains(body, "first-failed-attempt") ||
		strings.Contains(body, "previously saved work") {
		t.Fatalf("The comment must contain only the current failed attempt: %s", body)
	}
}

func TestFailureUpdatesTheResultWithoutErasingSavedReviewDetails(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "current details"
		if legacy {
			name = "details without a result row"
		}

		t.Run(
			name,
			func(t *testing.T) {
				options := serviceFixtureOptions{
					pullRequestStatus: http.StatusBadGateway,
				}
				fixture := newServiceFixture(t, options)
				summary := findingDetailsSummary()
				summary.Result = "previous failure result"
				summary.Usage.InputTokens = 4321
				body := review.RenderBody(summary)
				if legacy {
					body = strings.ReplaceAll(
						body,
						"| `result` | `previous failure result` |\n",
						"",
					)
				}

				state := marker.State{
					RunID:  "saved-review",
					Status: marker.StateReviewing,
				}
				fixture.state.issueComments = []map[string]any{
					{
						"id":   float64(2000),
						"user": map[string]any{"login": testBotLogin},
						"body": body + "\n" + marker.EncodeState(state),
					},
				}
				runErr := fixture.run(t.Context(), fixture.forcedJob())
				if runErr == nil {
					t.Fatal("The review must return the failed pull request read.")
				}

				body = failureSummaryComment(t, fixture)
				if strings.Count(body, "| `result` |") != 1 ||
					!strings.Contains(body, runErr.Error()) ||
					!strings.Contains(body, "| `input_tokens` | `4321` |") ||
					strings.Contains(body, "previous failure result") ||
					retainedMetadataCount(t, body, summary.Metadata[0].Body) != 1 {
					t.Fatalf(
						"The comment must update the result and retain saved review data: %s",
						body,
					)
				}
			},
		)
	}
}

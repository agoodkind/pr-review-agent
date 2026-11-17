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
		!strings.Contains(
			body,
			"PR Review Agent could not finish this review and has not scheduled a retry.",
		) ||
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
	if strings.Count(
		body,
		"PR Review Agent could not finish this review and has not scheduled a retry.",
	) != 1 ||
		strings.Count(body, "<summary>Review details</summary>") != 1 ||
		strings.Contains(body, "first-failed-attempt") ||
		strings.Contains(body, "previously saved work") {
		t.Fatalf("The comment must contain only the current failed attempt: %s", body)
	}
}

func TestFailedRefreshRemovesLegacyIncompleteNotice(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{pullRequestStatus: http.StatusBadGateway})
	summary := findingDetailsSummary()
	summary.Usage.InputTokens = 4321
	saved := rendered(t)(review.RenderFailureBody(summary, "", ""))
	_, saved, found := strings.Cut(saved, "<!-- pr-review-agent:maintenance-failure:end -->\n\n")
	if !found {
		t.Fatal("The saved review must contain its rendered sections.")
	}

	legacy := "## Review\n\n" +
		"1 chunk could not be reviewed on `a3c4f1c`. Apply the `test-review-agent-rerun` label to retry this head.\n\n" +
		"The model provider is unavailable. Wait for it to recover.\n\n" +
		testUnreviewedHeadReason + "\n\n" +
		"The cause is recorded in this service's log, under run identifier `old-run`.\n\n" +
		saved
	state := marker.State{
		RunID:   "old-run",
		Status:  marker.StateReviewing,
		Pending: []string{"pending"},
	}
	fixture.state.issueComments = []map[string]any{
		{
			"id":   float64(2000),
			"user": map[string]any{"login": testBotLogin},
			"body": legacy + "\n" + marker.EncodeState(state),
		},
	}

	if err := fixture.run(t.Context(), fixture.forcedJob()); err == nil {
		t.Fatal("The refresh must report the failed pull request read.")
	}

	body := failureSummaryComment(t, fixture)
	want := "PR Review Agent could not finish this review and has not scheduled a retry."
	if strings.Count(body, want) != 1 || strings.Contains(body, "label to retry this head") ||
		strings.Contains(body, "The model provider is unavailable") ||
		strings.Contains(
			body,
			"old-run",
		) || !strings.Contains(body, "| `input_tokens` | `4321` |") ||
		retainedMetadataCount(t, body, summary.Metadata[0].Body) != 1 {
		t.Fatalf("The refresh must replace the legacy notice and retain the review: %s", body)
	}

	current := decodedSummaryState(t, fixture)
	if len(current.Pending) != 1 || current.Pending[0] != "pending" {
		t.Fatalf("The refresh lost the pending checkpoint: %+v", current)
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
				body := rendered(t)(review.RenderBody(summary))
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

				job := fixture.forcedJob()
				job.DeliveryID = "second-saved-failure"
				if err := fixture.run(t.Context(), job); err == nil {
					t.Fatal("The second review must return the failed pull request read.")
				}

				body = failureSummaryComment(t, fixture)
				if strings.Count(
					body,
					"PR Review Agent could not finish this review and has not scheduled a retry.",
				) != 1 ||
					retainedMetadataCount(t, body, summary.Metadata[0].Body) != 1 {
					t.Fatalf(
						"The repeated failure must retain one notice and the saved finding: %s",
						body,
					)
				}
			},
		)
	}
}

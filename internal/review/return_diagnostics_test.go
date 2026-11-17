package review_test

import (
	"errors"
	"html"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestSuccessfulRefreshClearsThePreviousReturnedError(t *testing.T) {
	finding := metadataPresentationFinding()
	model := &sequenceModel{
		results: []domain.ReviewResult{{Findings: []domain.Finding{finding}}},
	}
	fixture, _ := newContentCollectionFixture(
		t,
		model,
		contentCollectionOptions{onlyMetadata: true, description: finding.Evidence},
	)
	job := fixture.job()
	if err := fixture.run(t.Context(), job); err != nil {
		t.Fatal(err)
	}

	original := failureSummaryComment(t, fixture)
	fixture.state.listThreadsStatus = http.StatusServiceUnavailable
	job.RefreshVerdict = true
	job.DeliveryID = "failed-verdict-refresh"
	if err := fixture.run(t.Context(), job); err == nil {
		t.Fatal("The thread read must fail the verdict refresh.")
	}

	failed := failureSummaryComment(t, fixture)
	if strings.Contains(failed, "| `result` | `null` |") {
		t.Fatal("The failed refresh must publish its returned error.")
	}

	fixture.state.listThreadsStatus = http.StatusOK
	job.DeliveryID = "recovered-verdict-refresh"
	if err := fixture.run(t.Context(), job); err != nil {
		t.Fatal(err)
	}

	body := failureSummaryComment(t, fixture)
	if !strings.Contains(body, "| `result` | `null` |") ||
		retainedMetadataCount(t, body, finding.Body) != 1 {
		t.Fatalf("The successful refresh must clear the error and retain its finding: %s", body)
	}

	_, originalDetails, found := strings.Cut(original, "<summary>Review details</summary>")
	if !found {
		t.Fatal("The initial review must publish its details.")
	}

	originalDetails, _, found = strings.Cut(originalDetails, "</details>")
	if !found || !strings.Contains(body, originalDetails) {
		t.Fatalf("The recovered refresh must retain the original review usage: %s", body)
	}
}

func TestFailureDetailsIncludeErrorsAfterTheReviewFailure(t *testing.T) {
	for _, scenario := range []string{"check completion", "verdict dismissal"} {
		t.Run(
			scenario,
			func(t *testing.T) {
				options := serviceFixtureOptions{
					reconcileErr: errors.New("reconciliation failed before review"),
				}
				if scenario == "check completion" {
					options.firstCheckCompletionStatus = http.StatusServiceUnavailable
				} else {
					options.reviewPages = botReviewPage("CHANGES_REQUESTED")
				}

				fixture := newServiceFixture(t, options)
				if scenario == "verdict dismissal" {
					fixture.state.dismissReviewStatus = http.StatusServiceUnavailable
				}

				runErr := fixture.run(t.Context(), fixture.job())
				if runErr == nil || !strings.Contains(runErr.Error(), "503") {
					t.Fatalf("The review must return its later publication failure: %v", runErr)
				}

				assertReturnedDiagnostic(t, failureSummaryComment(t, fixture), runErr)
			},
		)
	}
}

func TestFailureDetailsIncludeARecoveredSummaryPublicationError(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})
	summary := testSummary()
	fixture.state.issueComments = []map[string]any{
		{
			"id":   float64(2000),
			"user": map[string]any{"login": testBotLogin},
			"body": rendered(t)(
				review.RenderBody(summary),
			) + "\n" + marker.EncodeState(marker.State{
				LastReviewed: summary.Head,
				RunID:        "previous-completed-review",
				Status:       marker.StateDone,
			}),
		},
	}
	fixture.state.issueUpdateStatuses = []int{http.StatusServiceUnavailable, http.StatusOK}
	job, accepted, err := fixture.service.Admit(t.Context(), fixture.forcedJob())
	if err != nil || !accepted {
		t.Fatalf("The failed summary publication test must admit its review: %v", err)
	}

	err = fixture.service.Reject(t.Context(), job, errors.New("review queue is full"))
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("The rejection must retain the first summary publication error: %v", err)
	}

	assertReturnedDiagnostic(t, failureSummaryComment(t, fixture), err)
}

func assertReturnedDiagnostic(t *testing.T, body string, returned error) {
	t.Helper()
	_, value, found := strings.Cut(body, "| `result` | ")
	if !found {
		t.Fatalf("The review must include its result row: %s", body)
	}

	value, _, _ = strings.Cut(value, "\n")
	if strings.HasPrefix(value, "<pre>") {
		value = strings.TrimPrefix(value, "<pre>")
		value = strings.TrimSuffix(value, "</pre> |")
		value = html.UnescapeString(value)
	} else {
		value = strings.TrimSuffix(value, " |")
		value = strings.Trim(value, "`")
	}

	if value != returned.Error() {
		t.Fatalf(
			"The result must equal the returned error: result=%q returned=%q",
			value,
			returned.Error(),
		)
	}
}

package review_test

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestRetryableFailureRequiresStoredOutcome(t *testing.T) {
	reviewErr := errors.New("review could not finish")
	check := serviceFixtureOptions{reconcileErr: reviewErr}
	refresh := serviceFixtureOptions{submitReviewStatus: http.StatusInternalServerError}
	incomplete := serviceFixtureOptions{model: &sequenceModel{err: reviewErr}}

	for _, scenario := range []struct {
		name         string
		options      serviceFixtureOptions
		disposition  domain.AssessmentDisposition
		recoverStore bool
	}{
		{"failed check", check, domain.AssessmentFailed, false},
		{"failed check with recovered storage", check, domain.AssessmentFailed, true},
		{"failed refresh", refresh, domain.AssessmentFailed, false},
		{"failed refresh with recovered storage", refresh, domain.AssessmentFailed, true},
		{"incomplete review", incomplete, domain.AssessmentIncomplete, false},
	} {
		t.Run(
			scenario.name,
			func(t *testing.T) {
				head := domain.HeadSHA(testStaleHeadSHA)
				if scenario.options.submitReviewStatus != 0 {
					head = domain.HeadSHA(testHeadSHA)
				}

				scenario.options.reviewPages = blockingVerdictReviewPage(head, true)
				fixture := newServiceFixture(t, scenario.options)
				job, accepted, err := fixture.service.Admit(t.Context(), fixture.job())
				if err != nil || !accepted {
					t.Fatalf("The service could not admit the review: %v", err)
				}

				storeDir := filepath.Join(t.TempDir(), "outcomes")
				retryAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
				storePath := filepath.Join(storeDir, "outcome")
				ctx := review.WithOutcomeSink(
					t.Context(),
					func(
						_ context.Context,
						_ domain.ReviewJob,
						outcome domain.AssessmentOutcome,
					) (domain.AssessmentOutcome, error) {
						outcome.RetryAtMS = retryAt.UnixMilli()
						outcome.ReassessmentID = "retry-1"
						encoded := []byte(domain.EncodeAssessmentOutcome(outcome))
						storeErr := os.WriteFile(storePath, encoded, 0o600)
						if errors.Is(storeErr, fs.ErrNotExist) && scenario.recoverStore {
							if err := os.Mkdir(storeDir, 0o700); err != nil {
								t.Fatal(err)
							}
						}

						return outcome, storeErr
					},
				)

				outcome, err := fixture.service.RunWithOutcome(ctx, job)
				if !errors.Is(err, fs.ErrNotExist) || outcome.Disposition != scenario.disposition {
					t.Fatalf("The service returned outcome %+v and error %v.", outcome, err)
				}

				if scenario.options.submitReviewStatus == 0 && !errors.Is(err, reviewErr) {
					t.Fatalf("The service omitted the review error: %v", err)
				}

				if len(fixture.state.dismissals) != 1 ||
					fixture.state.dismissals[0]["review_id"] != "31" {
					t.Fatalf(
						"The service did not dismiss the previous rejection: %v",
						fixture.state.dismissals,
					)
				}

				body := failureSummaryComment(t, fixture)
				if scenario.disposition == domain.AssessmentIncomplete {
					state, found := marker.DecodeState(body)
					if !found || len(state.Pending) == 0 {
						t.Fatalf(
							"The incomplete summary omitted the pending review chunks: %s",
							body,
						)
					}
				}

				if !scenario.recoverStore {
					if fixture.state.lastUpdateCheckRun["status"] == "completed" {
						t.Fatal("The service completed the check before it stored the retry.")
					}

					if !strings.Contains(body, "has not scheduled a retry") ||
						!strings.Contains(body, "| `retry_at` | `null` |") {
						t.Fatalf("The summary reported a retry without stored intent: %s", body)
					}

					return
				}

				stored, err := os.ReadFile(storePath)
				if err != nil {
					t.Fatal(err)
				}

				if domain.DecodeAssessmentOutcome(string(stored)).RetryAtMS != outcome.RetryAtMS ||
					outcome.RetryAtMS != retryAt.UnixMilli() {
					t.Fatalf(
						"The review returned a different retry time from storage: %+v",
						outcome,
					)
				}

				if fixture.state.lastUpdateCheckRun["status"] != "completed" {
					t.Fatal("The service did not complete the check after it stored the retry.")
				}

				row := "| `retry_at` | `" + retryAt.Format(time.RFC3339) + "` |"
				checkSummary, _ := checkOutput(t, fixture)["summary"].(string)
				if !strings.Contains(checkSummary, row) || !strings.Contains(body, row) ||
					!strings.Contains(body, "will try again") {
					t.Fatalf(
						"The check or comment omitted the stored retry: %s, %s",
						checkSummary,
						body,
					)
				}
			},
		)
	}
}

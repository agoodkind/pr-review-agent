package review_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/reassessment"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestRetryOutcomePublication(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		enabled  bool
		declined bool
		retry    bool
	}{
		{"HTTP 530 disabled", false, false, true},
		{"HTTP 530 enabled", true, false, true},
		{"declined disabled", true, true, false},
		{"declined enabled", true, true, true},
	} {
		t.Run(
			scenario.name,
			func(t *testing.T) {
				options := serviceFixtureOptions{
					model: &failThenSucceedModel{err: &openai.ProviderError{StatusCode: 530}},
				}
				if scenario.declined {
					options = serviceFixtureOptions{
						collector:       twoChunkCollector{},
						reviewMaxChunks: 1,
					}
				}

				fixture := newServiceFixture(t, options)
				job := fixture.job()
				admitted, accepted, err := fixture.service.Admit(t.Context(), job)
				if err != nil || !accepted {
					t.Fatalf("admission failed: %v", err)
				}

				var stored *reassessment.Record
				server := httptest.NewServer(
					http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						var mutation struct {
							Action string               `json:"action"`
							Record *reassessment.Record `json:"record"`
						}
						if err := json.NewDecoder(request.Body).Decode(&mutation); err != nil {
							t.Error(err)

							return
						}

						switch mutation.Action {
						case "query":
							_ = json.NewEncoder(writer).Encode(reassessment.Response{
								Record: stored,
							})
						case "query_target":
							_, _ = writer.Write([]byte(`{"target":null}`))
						case "write_target":
							_, _ = writer.Write([]byte(`{"target":null,"applied":true}`))
						case "transition":
							stored = mutation.Record
							_ = json.NewEncoder(writer).Encode(reassessment.Response{
								Applied: true,
								Record:  stored,
							})
						default:
							t.Errorf(
								"The test server received an unexpected action: %q.",
								mutation.Action,
							)
						}
					}),
				)
				defer server.Close()
				coordinator := reassessment.Coordinator{
					Settings: config.Reassessment{
						Enabled:       scenario.enabled,
						RetryDeclined: scenario.declined && scenario.retry,
						InitialDelay:  "1m",
						MaximumDelay:  "1m",
					},
					Clock: func() time.Time {
						return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
					},
					Store:  &reassessment.Client{URL: server.URL, HTTP: server.Client()},
					Logger: slog.Default(),
				}
				ctx := review.WithOutcomeSink(t.Context(), coordinator.Observe)
				outcome, err := fixture.service.RunWithOutcome(ctx, admitted)
				if err != nil || outcome.Retryable == nil || *outcome.Retryable != scenario.retry {
					t.Fatalf("The review returned outcome %+v with error %v.", outcome, err)
				}

				body := failureSummaryComment(t, fixture)
				wantNotice := "PR Review Agent could not finish this review and has not scheduled a retry."
				if scenario.enabled && scenario.retry {
					wantNotice = "PR Review Agent could not finish this review and will try again."
				} else if !scenario.retry {
					wantNotice = "PR Review Agent hit a fatal error while reviewing this PR and cannot recover."
				}

				if !scenario.declined &&
					(!strings.Contains(body, "\n\n"+wantNotice+"\n\n") ||
						strings.Contains(body, "The queue will reassess")) {
					t.Fatalf("comment has an incorrect failure notice: %s", body)
				}

				want := "| `retry_at` | `null` |"
				if scenario.enabled && scenario.retry {
					if stored == nil || stored.Phase != "waiting" ||
						outcome.RetryAtMS != stored.NotBeforeMS {
						t.Fatalf(
							"outcome schedule differs from the stored record: %+v, %+v",
							outcome,
							stored,
						)
					}

					want = "| `retry_at` | `" + time.UnixMilli(stored.NotBeforeMS).UTC().Format(
						time.RFC3339,
					) + "` |"
				}

				wantRetryable := "| `retryable` | `false` |"
				if scenario.retry {
					wantRetryable = "| `retryable` | `true` |"
				}

				if !strings.Contains(body, wantRetryable) || !strings.Contains(body, want) {
					t.Fatalf("comment omitted retry outcome: %s", body)
				}
			},
		)
	}
}

func TestPanicPublishesFatalNotice(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{model: panicModel{}})
	job, accepted, err := fixture.service.Admit(t.Context(), fixture.job())
	if err != nil || !accepted {
		t.Fatalf("admission failed: %v", err)
	}

	outcome, err := fixture.service.RunWithOutcome(t.Context(), job)
	if err == nil || outcome.Retryable == nil || *outcome.Retryable {
		t.Fatalf("outcome = %+v, error = %v", outcome, err)
	}

	body := failureSummaryComment(t, fixture)
	want := "PR Review Agent hit a fatal error while reviewing this PR and cannot recover."
	if strings.Count(body, want) != 1 || !strings.Contains(body, "| `retryable` | `false` |") {
		t.Fatalf("comment has an incorrect fatal notice: %s", body)
	}
}

func TestFailedReviewReportsOutcomeWhenStorageFails(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{model: panicModel{}})
	job, accepted, err := fixture.service.Admit(t.Context(), fixture.job())
	if err != nil || !accepted {
		t.Fatalf("The service could not admit the review: %v", err)
	}

	storageErr := errors.New("outcome storage is unavailable")
	ctx := review.WithOutcomeSink(
		t.Context(),
		func(_ context.Context, _ domain.ReviewJob, outcome domain.AssessmentOutcome) (
			domain.AssessmentOutcome,
			error,
		) {
			return outcome, storageErr
		},
	)
	_, err = fixture.service.RunWithOutcome(ctx, job)
	if !errors.Is(err, storageErr) || fixture.state.lastUpdateCheckRun["status"] != "completed" ||
		!strings.Contains(failureSummaryComment(t, fixture), "fatal error") {
		t.Fatalf(
			"The review must report the storage error and publish the failure: %v, %v",
			err,
			fixture.state.lastUpdateCheckRun,
		)
	}
}

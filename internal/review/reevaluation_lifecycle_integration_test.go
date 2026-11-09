package review_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/reconcile"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestFailedMetadataRefreshVerifiesInlineFindingsBeforeDismissal(t *testing.T) {
	for _, scenario := range []struct {
		name           string
		outdated       bool
		failThreadRead bool
		resolved       bool
		modelDelay     time.Duration
		success        bool
		unchanged      bool
	}{
		{name: "model exceeds publication budget", modelDelay: 65 * time.Second, success: true},
		{name: "current reassessment usage", success: true},
		{name: "unchanged metadata avoids provider work", success: true, unchanged: true},
		{name: "open file finding"},
		{name: "outdated file finding", outdated: true},
		{name: "failed thread verification", failThreadRead: true},
		{name: "resolved file finding", resolved: true},
	} {
		t.Run(
			scenario.name,
			func(t *testing.T) {
				var (
					refreshing atomic.Bool
					requests   atomic.Int32
					fixture    *serviceFixture
				)

				provider := httptest.NewServer(
					http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						var payload struct {
							Text struct {
								Format struct {
									Name metadataFallbackStage `json:"name"`
								} `json:"format"`
							} `json:"text"`
						}
						if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
							http.Error(writer, err.Error(), http.StatusBadRequest)

							return
						}

						requestIndex := int(requests.Add(1))

						if refreshing.Load() {
							if scenario.success {
								time.Sleep(scenario.modelDelay)
								writeInspectionServiceCompletion(
									writer,
									`{"findings":[],"omissions_acceptable":true,"inspection_requests":[]}`,
									requestIndex,
								)

								return
							}

							if scenario.failThreadRead {
								fixture.state.mu.Lock()
								fixture.state.listThreadsStatus = http.StatusServiceUnavailable
								fixture.state.mu.Unlock()
							}

							writeInspectionServiceCompletion(writer, "{", requestIndex)

							return
						}

						switch payload.Text.Format.Name {
						case metadataFallbackReview:
							writeInspectionServiceCompletion(
								writer,
								`{"findings":[],"omissions_acceptable":true,"inspection_requests":[]}`,
								requestIndex,
							)
						case metadataFallbackReport:
							writeInspectionServiceCompletion(
								writer,
								`{"summary":"The service preserves unresolved findings.","walkthrough":[]}`,
								requestIndex,
							)
						default:
							http.Error(
								writer,
								"The fixture does not support this model stage.",
								http.StatusBadRequest,
							)
						}
					}),
				)
				t.Cleanup(provider.Close)

				address, err := url.Parse(provider.URL)
				if err != nil {
					t.Fatal(err)
				}

				policy := policytest.Load(t)
				model := openai.NewClient(
					config.Config{
						Providers: []config.ProviderConfig{
							{
								ID:      "lifecycle",
								API:     config.ResponsesAPI,
								Model:   "lifecycle",
								APIKey:  "example-credential",
								BaseURL: address,
							},
						},
						ProviderFailurePolicy: config.ProviderAnyFailure,
						ReviewPolicy:          policy,
						MinimumImportance:     testMinimumImportance,
					},
					provider.Client(),
				)
				fixture, _ = newContentCollectionFixture(
					t,
					model,
					contentCollectionOptions{
						onlyMetadata: true,
						currentDescription: func() string {
							if refreshing.Load() && !scenario.unchanged {
								return "The current description still requires review."
							}

							return metadataFallbackDescription
						},
					},
				)
				thread := botThreadAt("standing-file-finding", 2, false)
				thread.Outdated = scenario.outdated
				fixture.state.threadNodes = threadNodesFor([]githubapp.ReviewThread{thread})
				logger := slog.New(slog.DiscardHandler)
				fixture.service = review.NewService(
					fixture.github,
					diff.NewCollector(fixture.github),
					model,
					reconcile.NewService(
						fixture.github,
						model,
						testBotLogin,
						logger,
						config.MaximumPromptBytes,
						policy,
					),
					queue.NewKeyedLocker(),
					testBotLogin,
					testMinimumImportance,
					config.DefaultReviewMaxFiles,
					config.DefaultReviewMaxChunks,
					config.DefaultReviewChunkTimeout,
					nil,
					testClock(8*time.Second),
					logger,
					1,
					config.MaximumPromptBytes,
					policy,
				)
				fixture.service.SetFailureVerdictPolicy(config.DismissLatestFailureVerdict)

				job := fixture.job()
				if err := fixture.run(t.Context(), job); err != nil {
					t.Fatal(err)
				}

				assertMetadataRejectionStillStands(t, fixture)
				original := fixture.state.submittedReviews[0]
				original["body"] = ""

				useLegacyMetadataCheckpoint(t, fixture)
				summaryID := fixture.state.issueComments[0]["id"]
				initialRequests := requests.Load()

				if scenario.resolved {
					thread.Resolved = true
					fixture.state.threadNodes = threadNodesFor([]githubapp.ReviewThread{thread})
				}

				refreshing.Store(true)

				job.DeliveryID = "failed-metadata-inline-verification"
				job.MetadataOnly = true

				admitted, accepted, err := fixture.service.Admit(t.Context(), job)
				if err != nil || !accepted {
					t.Fatalf("The description edit did not admit reassessment: %v.", err)
				}

				outcome, err := fixture.service.RunWithOutcome(t.Context(), admitted)
				if scenario.success {
					if err != nil || outcome.Disposition != domain.AssessmentCompleted ||
						fixture.state.lastUpdateCheckRun["conclusion"] != "success" {
						t.Fatalf(
							"The valid model response must publish after its delay: %+v, %v.",
							outcome,
							err,
						)
					}
				} else if err == nil || outcome.Disposition != domain.AssessmentFailed || fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
					t.Fatalf(
						"The malformed provider response must fail its check: %+v, %v.",
						outcome,
						err,
					)
				}

				if scenario.resolved {
					if original["state"] != "DISMISSED" || len(fixture.state.dismissals) != 1 {
						t.Fatal(
							"The failure policy must withdraw a rejection after its file findings resolve.",
						)
					}
				} else if original["state"] != "CHANGES_REQUESTED" || len(
					fixture.state.dismissals,
				) != 0 {
					t.Fatal(
						"The failure policy withdrew a rejection without proving its file findings resolved.",
					)
				}

				if len(fixture.state.submittedReviews) != 1 || original["body"] != "" || len(
					fixture.state.issueComments,
				) != 1 || fixture.state.issueComments[0]["id"] != summaryID {
					t.Fatal("The failed refresh replaced the standing review or original summary.")
				}

				body := failureSummaryComment(t, fixture)
				if len(
					retainedMetadataFindings(t, body),
				) != 0 || decodedSummaryState(t, fixture).LastReviewed != job.Head {
					t.Fatal(
						"The failed metadata refresh changed saved findings or completed file coverage.",
					)
				}

				output, valid := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
				if !valid {
					t.Fatal("The current check did not publish its output.")
				}

				checkSummary, valid := output["summary"].(string)
				if !valid {
					t.Fatal("The current check did not publish its summary.")
				}

				expectedRequests := int32(1)
				if scenario.unchanged {
					expectedRequests = 0
					if output["title"] != "Already reviewed" {
						t.Fatal(
							"A refresh without provider work must retain the duplicate-suppression result.",
						)
					}
				} else {
					assertMetadataRefreshUsage(t, checkSummary, expectedRequests)
					if scenario.success && output["title"] != "Review refreshed" {
						t.Fatal(
							"A completed model reassessment must identify its current check as refreshed.",
						)
					}
				}

				assertMetadataRefreshUsage(t, body, initialRequests)
				if requests.Load()-initialRequests != expectedRequests {
					t.Fatal("The refresh completed an unexpected number of provider requests.")
				}

				fixture.state.listThreadsStatus = http.StatusOK

				threads, err := fixture.github.ListReviewThreads(
					t.Context(),
					job.InstallationID,
					job.Repository,
					job.Number,
				)
				if err != nil || len(
					threads,
				) != 1 || threads[0].Resolved != scenario.resolved || threads[0].Outdated != scenario.outdated {
					t.Fatalf("The failed refresh changed the file thread: %+v, %v.", threads, err)
				}
			},
		)
	}
}

func assertMetadataRefreshUsage(t *testing.T, body string, requests int32) {
	t.Helper()

	for _, row := range []string{
		"| Requests | `" + strconv.Itoa(int(requests)) + "` |",
		"| Requests reporting usage | `" + strconv.Itoa(int(requests)) + "` |",
		"| Input tokens | `" + strconv.Itoa(int(requests)*11) + "` |",
		"| Output tokens | `" + strconv.Itoa(int(requests)*5) + "` |",
		"| Total tokens | `" + strconv.Itoa(int(requests)*16) + "` |",
	} {
		if !strings.Contains(body, row) {
			t.Fatalf("The refresh lost recorded usage: %s.\n%s", row, body)
		}
	}
}

func useLegacyMetadataCheckpoint(t *testing.T, fixture *serviceFixture) {
	t.Helper()
	state := decodedSummaryState(t, fixture)
	previous := marker.EncodeState(state)
	state.PolicyRevision = ""
	body := failureSummaryComment(t, fixture)
	fixture.state.issueComments[0]["body"] = strings.Replace(
		body,
		previous,
		marker.EncodeState(state),
		1,
	)
}

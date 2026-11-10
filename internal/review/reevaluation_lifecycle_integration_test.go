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
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestFailedMetadataRefreshVerifiesInlineFindingsBeforeDismissal(t *testing.T) {
	for _, scenario := range []struct {
		name               string
		outdated           bool
		failThreadRead     bool
		resolved           bool
		modelDelay         time.Duration
		success            bool
		unchanged          bool
		moveHead           bool
		publicationFailure string
	}{
		{name: "model exceeds publication budget", modelDelay: 65 * time.Second, success: true},
		{name: "current reassessment usage", success: true},
		{name: "unchanged metadata avoids provider work", success: true, unchanged: true},
		{name: "head moves during model work", success: true, moveHead: true},
		{name: "checkpoint publication fails", success: true, publicationFailure: "checkpoint"},
		{name: "native publication fails", success: true, publicationFailure: "native"},
		{name: "summary publication fails", success: true, publicationFailure: "summary"},
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
								fixture.state.mu.Lock()
								if scenario.moveHead {
									fixture.state.headSHA = testHeadSHA
								}

								switch scenario.publicationFailure {
								case "checkpoint":
									fixture.state.issueUpdateStatuses = []int{
										http.StatusServiceUnavailable,
									}
								case "native":
									fixture.state.submitReviewStatus = http.StatusServiceUnavailable
								case "summary":
									fixture.state.issueUpdateStatuses = []int{
										http.StatusOK,
										http.StatusServiceUnavailable,
									}
								}

								fixture.state.mu.Unlock()

								result := domain.ReviewResult{OmissionsAcceptable: true}

								if scenario.publicationFailure != "" {
									finding := metadataPresentationFinding()
									finding.Claim = "The description omits a subject and verb."
									result.Findings = []domain.Finding{finding}
								}

								response, encodeErr := json.Marshal(
									struct {
										domain.ReviewResult
										InspectionRequests []review.InspectionRequest `json:"inspection_requests"`
									}{
										ReviewResult:       result,
										InspectionRequests: []review.InspectionRequest{},
									},
								)
								if encodeErr != nil {
									http.Error(
										writer,
										encodeErr.Error(),
										http.StatusInternalServerError,
									)

									return
								}

								writeInspectionServiceCompletion(
									writer,
									string(response),
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
								if scenario.publicationFailure != "" {
									return "Summary changes"
								}

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
					time.Now,
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

				if scenario.publicationFailure != "" {
					current, readErr := fixture.github.GetPullRequest(
						t.Context(),
						job.InstallationID,
						job.Repository,
						job.Number,
					)
					if readErr != nil {
						t.Fatal(readErr)
					}

					saved := testSummary()
					saved.Head = job.Head
					saved.Decision = domain.ReviewDecisionRequestChanges
					saved.MetadataRevision = diff.PullRequestRevision(current)
					saved.FileAssessmentMaximumBytes = policy.FileAssessmentMaximumBytes()
					saved.FileAssessmentsDisplay = policy.FileAssessmentsDisplay()
					saved.FileAssessments = []domain.FileAssessment{
						{
							Path:                     "main.go",
							Identity:                 "stored-source-assessment",
							ContentKind:              reviewrules.ContentSource,
							ContentReviewRequirement: domain.ContentReviewRequired,
							Action:                   domain.FileActionReview,
							Reviewed:                 true,
							Reason:                   "The source review inspected the changed behavior.",
							Evidence: []string{
								"The source review inspected main.go.",
							},
						},
					}
					saved.Usage = review.UsageSummary{
						Requests:         int(initialRequests),
						ReportedRequests: int(initialRequests),
						InputTokens:      int64(initialRequests) * 11,
						OutputTokens:     int64(initialRequests) * 5,
						TotalTokens:      int64(initialRequests) * 16,
					}
					state := decodedSummaryState(t, fixture)
					fixture.state.issueComments[0]["body"] = review.RenderBody(
						saved,
					) + "\n" + marker.EncodeState(state)
				}

				priorBody := failureSummaryComment(t, fixture)
				priorUpdates := fixture.state.issueCommentUpdates

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
				if scenario.moveHead {
					if err != nil || outcome.Disposition != domain.AssessmentInterrupted ||
						fixture.state.lastUpdateCheckRun["conclusion"] != "cancelled" {
						t.Fatalf("A head change must cancel the refresh: %+v, %v.", outcome, err)
					}

					if failureSummaryComment(t, fixture) != priorBody ||
						fixture.state.issueCommentUpdates != priorUpdates ||
						fixture.state.lastUpdateReview != nil {
						t.Fatal("A cancelled refresh must not publish a stale verdict or summary.")
					}
				} else if scenario.publicationFailure != "" {
					if err == nil || outcome.Disposition != domain.AssessmentFailed ||
						fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
						t.Fatalf(
							"A publication error must fail the refresh: %+v, %v.",
							outcome,
							err,
						)
					}
				} else if scenario.success {
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

				expectedReviews := 1
				if scenario.publicationFailure == "summary" {
					expectedReviews = 2
				}

				if len(fixture.state.submittedReviews) != expectedReviews ||
					original["body"] != "" ||
					len(fixture.state.issueComments) != 1 ||
					fixture.state.issueComments[0]["id"] != summaryID {
					t.Fatal("The failed refresh replaced the standing review or original summary.")
				}

				body := failureSummaryComment(t, fixture)
				stored := retainedMetadataFindings(t, body)

				expectedMetadata := 0
				if scenario.publicationFailure == "native" ||
					scenario.publicationFailure == "summary" {
					expectedMetadata = 1
				}

				if len(stored) != expectedMetadata ||
					decodedSummaryState(t, fixture).LastReviewed != job.Head {
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

				if scenario.moveHead || scenario.publicationFailure != "" {
					for _, coverage := range []string{
						"| All file content inspected | yes |",
						"| Review scope assessed | yes |",
					} {
						if !strings.Contains(checkSummary, coverage) {
							t.Fatalf(
								"The current check must retain the completed assessment: %s.",
								coverage,
							)
						}
					}
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

					if !strings.Contains(
						checkSummary,
						"| Model | `lifecycle`, `inspection-service-model` |",
					) {
						t.Fatal(
							"The current check must identify the requested and reported models.",
						)
					}

					if scenario.publicationFailure != "" &&
						!strings.Contains(
							checkSummary,
							"The source review inspected the changed behavior.",
						) {
						t.Fatal("The failed check must retain the computed file assessment.")
					}

					if scenario.success && !scenario.moveHead &&
						!strings.Contains(checkSummary, "| Bot threads open | `1` |") {
						t.Fatal("The current check must reuse the computed file-thread assessment.")
					}

					if scenario.success && !scenario.moveHead &&
						scenario.publicationFailure == "" &&
						output["title"] != "Review refreshed" {
						t.Fatal(
							"A completed model reassessment must identify its current check as refreshed.",
						)
					}

					if scenario.moveHead && !strings.Contains(checkSummary, string(job.Head)[:7]) {
						t.Fatal("The cancelled check must identify the attempted head.")
					}
				}

				if scenario.modelDelay > 0 {
					_, duration, found := strings.Cut(checkSummary, "| Duration | `")
					secondsText, _, complete := strings.Cut(duration, "` seconds |")

					seconds, parseErr := strconv.Atoi(secondsText)
					if !found || !complete || parseErr != nil ||
						time.Duration(seconds)*time.Second < scenario.modelDelay {
						t.Fatalf(
							"The current check must report the measured model delay: %s.",
							secondsText,
						)
					}

					t.Logf("The current check reports a duration of %d seconds.", seconds)
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

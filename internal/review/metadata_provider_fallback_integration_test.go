package review_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/reconcile"
	"goodkind.io/pr-review-agent/internal/review"
)

const metadataFallbackDescription = "Review retries preserve saved findings until the current description establishes a correction."

type metadataFallbackStage string

const (
	metadataFallbackReview      metadataFallbackStage = "review_result"
	metadataFallbackReport      metadataFallbackStage = "review_report"
	metadataFallbackAssessments metadataFallbackStage = "metadata_finding_assessments"
	metadataFallbackConsolidate metadataFallbackStage = "finding_consolidation"
	metadataFallbackPrimary                           = "primary"
	metadataFallbackSecondary                         = "secondary"
)

type metadataFallbackCapture struct {
	mu                 sync.Mutex
	initial            bool
	description        string
	requests           int
	assessmentAttempts []string
}

func TestMetadataProviderFallbackPreservesUsageAndTheStandingDecision(t *testing.T) {
	for _, scenario := range []struct {
		name              string
		policy            config.ProviderFailurePolicy
		resolution        domain.Resolution
		invalidSecondary  bool
		emptyNativeBody   bool
		expectFailure     bool
		expectApproval    bool
		expectSecondCalls int
	}{
		{
			name:              "grounded resolution",
			policy:            config.ProviderAnyFailure,
			resolution:        domain.ResolutionResolved,
			expectApproval:    true,
			expectSecondCalls: 1,
		},
		{
			name:              "grounded open finding",
			policy:            config.ProviderAnyFailure,
			resolution:        domain.ResolutionOpen,
			expectSecondCalls: 1,
		},
		{
			name:              "every provider returns stale evidence",
			policy:            config.ProviderAnyFailure,
			resolution:        domain.ResolutionResolved,
			invalidSecondary:  true,
			expectFailure:     true,
			expectSecondCalls: 1,
		},
		{
			name:              "saved finding with empty native body",
			policy:            config.ProviderAnyFailure,
			resolution:        domain.ResolutionResolved,
			invalidSecondary:  true,
			emptyNativeBody:   true,
			expectFailure:     true,
			expectSecondCalls: 1,
		},
		{
			name:          "recoverable policy stops at invalid evidence",
			policy:        config.ProviderRecoverableFailure,
			resolution:    domain.ResolutionResolved,
			expectFailure: true,
		},
	} {
		t.Run(
			scenario.name,
			func(t *testing.T) {
				capture := &metadataFallbackCapture{
					initial:     true,
					description: metadataFallbackDescription + "\n\n## Testing\n\nUnit tests passed.",
				}
				finding := domain.Finding{
					Surface:    domain.FindingPullRequestDescription,
					RuleID:     "pr_material_verification",
					StartLine:  3,
					EndLine:    5,
					Title:      "Remove routine test narration",
					Body:       "The Testing section reports only unit tests. Remove this routine test narration.",
					Claim:      "The description reports routine unit-test results.",
					Evidence:   "Unit tests passed.",
					Importance: 10,
				}

				var fixture *serviceFixture

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

						capture.mu.Lock()
						defer capture.mu.Unlock()

						capture.requests++

						var response []byte

						var err error

						switch payload.Text.Format.Name {
						case metadataFallbackReview:
							result := domain.ReviewResult{OmissionsAcceptable: true}
							if capture.initial {
								result.Findings = []domain.Finding{finding}
							}

							response, err = json.Marshal(
								struct {
									domain.ReviewResult
									InspectionRequests []review.InspectionRequest `json:"inspection_requests"`
								}{
									ReviewResult:       result,
									InspectionRequests: []review.InspectionRequest{},
								},
							)
						case metadataFallbackReport:
							response, err = json.Marshal(review.Report{
								Summary: metadataFallbackDescription,
								Walkthrough: []string{
									"The service updates the original summary comment during reassessment.",
								},
							})
						case metadataFallbackConsolidate:
							response = []byte(`{"groups":[]}`)
						case metadataFallbackAssessments:
							selected := metadataFallbackPrimary
							if strings.HasPrefix(request.URL.Path, "/secondary/") {
								selected = metadataFallbackSecondary
							}

							capture.assessmentAttempts = append(
								capture.assessmentAttempts,
								selected,
							)

							assertMetadataRejectionStillStands(t, fixture)

							assessment := domain.MetadataFindingAssessment{
								FindingID:  "1",
								Resolution: scenario.resolution,
								Reason:     "The current description omits the routine testing section.",
								Evidence:   metadataFallbackDescription,
								StartLine:  1,
								EndLine:    1,
							}
							if scenario.resolution != domain.ResolutionResolved {
								assessment.Evidence = "Unit tests passed."
								assessment.StartLine, assessment.EndLine = 3, 3
								assessment.Reason = "The description still reports routine unit-test results after removing the Testing heading. Remove the test narration."
							}

							if selected == metadataFallbackPrimary || scenario.invalidSecondary {
								assessment.Evidence = "## Testing"
								assessment.StartLine, assessment.EndLine = 1, 1
							}

							response, err = json.Marshal(
								struct {
									Assessments []domain.MetadataFindingAssessment `json:"assessments"`
								}{
									Assessments: []domain.MetadataFindingAssessment{assessment},
								},
							)
						default:
							http.Error(
								writer,
								"The fixture does not support this model stage.",
								http.StatusBadRequest,
							)

							return
						}

						if err != nil {
							http.Error(writer, err.Error(), http.StatusInternalServerError)

							return
						}

						writeInspectionServiceCompletion(writer, string(response), capture.requests)
					}),
				)
				t.Cleanup(provider.Close)

				providers := []config.ProviderConfig{}

				for _, identifier := range []string{
					metadataFallbackPrimary,
					metadataFallbackSecondary,
				} {
					address, err := url.Parse(provider.URL + "/" + identifier)
					if err != nil {
						t.Fatal(err)
					}

					providers = append(
						providers,
						config.ProviderConfig{
							ID:      identifier,
							API:     config.ResponsesAPI,
							Model:   identifier,
							APIKey:  "example-credential",
							BaseURL: address,
						},
					)
				}

				policy := policytest.Load(t)
				model := openai.NewClient(
					config.Config{
						Providers:             providers,
						ProviderFailurePolicy: scenario.policy,
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
							capture.mu.Lock()
							defer capture.mu.Unlock()

							return capture.description
						},
					},
				)
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
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
				job := fixture.job()
				admitted, accepted, err := fixture.service.Admit(t.Context(), job)

				if err != nil || !accepted {
					t.Fatalf("The initial metadata review did not admit its job: %v.", err)
				}

				outcome, err := fixture.service.RunWithOutcome(t.Context(), admitted)
				if err != nil || outcome.Disposition != domain.AssessmentCompleted {
					t.Fatalf("The initial review did not complete: %+v, %v.", outcome, err)
				}

				assertMetadataRejectionStillStands(t, fixture)
				initialSummaryID := fixture.state.issueComments[0]["id"]
				originalRejection := fixture.state.submittedReviews[0]
				if scenario.emptyNativeBody {
					originalRejection["body"] = ""
				}

				capture.mu.Lock()
				initialRequests := capture.requests
				capture.initial = false
				capture.description = metadataFallbackDescription

				if scenario.resolution != domain.ResolutionResolved {
					capture.description += "\n\nUnit tests passed."
				}

				capture.mu.Unlock()

				job.DeliveryID = "metadata-current-field-reassessment"
				job.MetadataOnly = true

				admitted, accepted, err = fixture.service.Admit(t.Context(), job)

				if err != nil || !accepted {
					t.Fatalf("The description edit did not admit reassessment: %v.", err)
				}

				outcome, err = fixture.service.RunWithOutcome(t.Context(), admitted)
				if scenario.expectFailure {
					if err == nil || outcome.Disposition != domain.AssessmentFailed ||
						fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
						t.Fatalf(
							"Invalid metadata evidence must fail the check: %+v, %v.",
							outcome,
							err,
						)
					}
				} else if err != nil || outcome.Disposition != domain.AssessmentCompleted || fixture.state.lastUpdateCheckRun["conclusion"] != "success" {
					t.Fatalf(
						"The valid fallback assessment did not complete: %+v, %v.",
						outcome,
						err,
					)
				}

				capture.mu.Lock()
				attempts := append([]string(nil), capture.assessmentAttempts...)
				requests := capture.requests - initialRequests
				capture.mu.Unlock()

				if len(attempts) != 1+scenario.expectSecondCalls ||
					attempts[0] != metadataFallbackPrimary ||
					scenario.expectSecondCalls == 1 && attempts[1] != metadataFallbackSecondary {
					t.Fatalf(
						"The configured failure policy selected unexpected providers: %v.",
						attempts,
					)
				}

				body := failureSummaryComment(t, fixture)
				usageBody := body

				if scenario.expectFailure {
					checkOutput, valid := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
					if !valid {
						t.Fatal("The failed check did not publish its usage details.")
					}

					usageBody, valid = checkOutput["summary"].(string)
					if !valid {
						t.Fatal("The failed check did not publish its summary.")
					}
				}

				for _, row := range []string{
					"| Requests | `" + strconv.Itoa(requests) + "` |",
					"| Requests reporting usage | `" + strconv.Itoa(requests) + "` |",
					"| Input tokens | `" + strconv.Itoa(requests*11) + "` |",
					"| Output tokens | `" + strconv.Itoa(requests*5) + "` |",
					"| Total tokens | `" + strconv.Itoa(requests*16) + "` |",
				} {
					if !strings.Contains(usageBody, row) {
						t.Fatalf(
							"The summary lost usage from a provider response: %s.\n%s",
							row,
							usageBody,
						)
					}
				}

				if len(fixture.state.issueComments) != 1 ||
					fixture.state.issueComments[0]["id"] != initialSummaryID {
					t.Fatal("Metadata reassessment replaced the original top-level summary.")
				}

				stored := retainedMetadataFindings(t, body)

				reviews, err := fixture.github.ListReviews(
					t.Context(),
					job.InstallationID,
					job.Repository,
					job.Number,
				)
				if err != nil {
					t.Fatal(err)
				}

				standing := githubapp.LatestDecisionReview(reviews, testBotLogin)
				if scenario.expectApproval {
					if standing.State != "APPROVED" || len(stored) != 0 {
						t.Fatal(
							"Grounded resolution did not clear the obsolete rejection and saved finding.",
						)
					}
				} else if standing.State != "CHANGES_REQUESTED" || originalRejection["state"] != "CHANGES_REQUESTED" || len(
					fixture.state.dismissals,
				) != 0 || len(stored) != 1 || fixture.state.lastSubmitReview["event"] == string(
					domain.ReviewDecisionApprove,
				) {
					t.Fatal(
						"The review withdrew its standing rejection without a grounded resolution.",
					)
				}
			},
		)
	}
}

func assertMetadataRejectionStillStands(t *testing.T, fixture *serviceFixture) {
	t.Helper()
	fixture.state.mu.Lock()
	defer fixture.state.mu.Unlock()

	if len(fixture.state.submittedReviews) != 1 ||
		fixture.state.submittedReviews[0]["state"] != "CHANGES_REQUESTED" ||
		len(fixture.state.dismissals) != 0 ||
		len(fixture.state.issueComments) != 1 {
		t.Error(
			"The service changed the standing rejection before the provider established a current-field resolution.",
		)
	}
}

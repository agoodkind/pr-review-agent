package review_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestResumedReviewTracesMetadataAfterACosmeticDescriptionEdit(t *testing.T) {
	for _, metadataOnly := range []bool{false, true} {
		name := "ordinary"
		if metadataOnly {
			name = "metadata edit"
		}

		t.Run(
			name,
			func(t *testing.T) {
				prior := metadataPresentationFinding()
				finding := prior
				finding.StartLine, finding.EndLine = 3, 3
				current := "## Changes\n\n" + finding.Evidence
				assessment := metadataDecision("1", domain.ResolutionOpen, finding.Evidence)
				assessment[0].StartLine, assessment[0].EndLine = 3, 3
				assessment[0].Reason = finding.Body
				model := &sequenceModel{
					results: []domain.ReviewResult{
						{Findings: []domain.Finding{finding}},
					},
					metadataAssessments: [][]domain.MetadataFindingAssessment{assessment},
				}
				fixture, _ := newContentCollectionFixture(
					t,
					model,
					contentCollectionOptions{
						description:  current,
						onlyMetadata: true,
					},
				)
				var logs bytes.Buffer
				logger := slog.New(slog.NewJSONHandler(&logs, nil))
				fixture.service = review.NewService(
					fixture.github,
					diff.NewCollector(fixture.github),
					model,
					fixture.reconciler,
					queue.NewKeyedLocker(),
					testBotLogin,
					testMinimumImportance,
					config.DefaultReviewMaxFiles,
					config.DefaultReviewMaxChunks,
					config.DefaultReviewChunkTimeout,
					nil,
					testClock(8*time.Second),
					logger,
					config.MaximumChunkConcurrency,
					config.MaximumPromptBytes,
					policytest.Load(t),
				)
				seedMetadataReassessment(t, fixture, []domain.Finding{prior}, false)
				priorSummary := findingDetailsSummary()
				priorSummary.Head = domain.HeadSHA(testStaleHeadSHA)
				priorSummary.Metadata = []domain.Finding{prior}
				fixture.state.reviewPages = [][]map[string]any{
					{
						{
							"id":        float64(4100),
							"commit_id": testStaleHeadSHA,
							"state":     "CHANGES_REQUESTED",
							"body":      review.RenderVerdictBody(priorSummary),
							"user":      map[string]any{"login": testBotLogin},
						},
					},
				}
				job := fixture.job()
				job.MetadataOnly = metadataOnly
				if err := fixture.run(t.Context(), job); err != nil {
					t.Fatalf(
						"The resumed review must process valid metadata findings after the heading edit: %v.",
						err,
					)
				}

				if strings.Contains(logs.String(), "path is empty") {
					t.Fatal("The resumed review emitted a metadata tracing failure: path is empty.")
				}

				if !strings.Contains(logs.String(), `"msg":"review analysis classified"`) ||
					!strings.Contains(logs.String(), `"surface":"pull_request_description"`) {
					t.Fatal(
						"The resumed review must trace the observed metadata finding with its actual source surface.",
					)
				}

				stored := retainedMetadataFindings(t, failureSummaryComment(t, fixture))
				if len(stored) != 1 || stored[0].Body != finding.Body || stored[0].StartLine != 3 ||
					stored[0].Suggestion != finding.Suggestion ||
					len(model.metadataPrompts) != 1 {
					t.Fatalf(
						"The resumed review must explicitly reassess and retain the relocated metadata finding: %+v.",
						stored,
					)
				}

				reviews, err := fixture.github.ListReviews(
					t.Context(),
					job.InstallationID,
					job.Repository,
					job.Number,
				)
				if err != nil {
					t.Fatal(err)
				}

				native := githubapp.LatestDecisionReview(reviews, testBotLogin)
				if native.State != "CHANGES_REQUESTED" ||
					!strings.Contains(native.Body, finding.Body) ||
					!strings.Contains(native.Body, finding.Suggestion) ||
					len(fixture.state.dismissals) != 0 ||
					fixture.state.lastUpdateCheckRun["conclusion"] != "success" {
					t.Fatal(
						"The resumed review must complete with an actionable native rejection instead of a tracing failure or dismissal.",
					)
				}
			},
		)
	}
}

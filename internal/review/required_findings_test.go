package review_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func metadataPresentationFinding() domain.Finding {
	return domain.Finding{
		Surface:          domain.FindingPullRequestDescription,
		RuleID:           "complete_sentences",
		StartLine:        1,
		EndLine:          1,
		Title:            "Complete the description",
		Body:             "The description omits a subject and verb. Write a complete sentence that explains the summary change beneath `### Summary`.",
		Evidence:         "Summary changes",
		Importance:       10,
		CorrectionAction: domain.CorrectionReplace,
		Suggestion:       "The summary displays required findings before the report.",
	}
}

func TestServicePresentsRequiredMetadataInTheNativeReview(t *testing.T) {
	finding := metadataPresentationFinding()
	titleFinding := domain.Finding{
		Surface:          domain.FindingPullRequestTitle,
		RuleID:           "pr_title",
		StartLine:        1,
		EndLine:          1,
		Title:            "Identify the changed review behavior",
		Body:             "The title does not identify the changed review behavior. State that behavior in the title.",
		Evidence:         "title",
		Importance:       10,
		CorrectionAction: domain.CorrectionReplace,
		Suggestion:       "Show metadata findings in native rejection reviews",
	}
	inline := severeFinding()
	model := &sequenceModel{
		results: []domain.ReviewResult{{Findings: []domain.Finding{finding, titleFinding, inline}}},
	}

	fixture, _ := newContentCollectionFixture(
		t,
		model,
		contentCollectionOptions{
			description: finding.Evidence,
		},
	)
	if err := fixture.run(t.Context(), fixture.job()); err != nil {
		t.Fatal(err)
	}

	if len(fixture.state.issueComments) != 1 || len(fixture.state.submittedReviews) != 2 ||
		len(fixture.state.threadNodes) != 1 ||
		len(fixture.state.streamedComments) != 1 {
		t.Fatal(
			"The mixed review must publish one summary, one inline rejection, and one metadata rejection.",
		)
	}

	summaryID := fixture.state.issueComments[0]["id"]
	inlineReviewID := fixture.state.submittedReviews[0]["id"]
	nativeID := fixture.state.submittedReviews[1]["id"]
	threadID := fixture.state.threadNodes[0]["id"]
	inlineBody := fmt.Sprint(fixture.state.streamedComments[0]["body"])

	const (
		reportStart = "<!-- pr-review-agent:report-section:start -->"
		reportEnd   = "<!-- pr-review-agent:report-section:end -->"
	)

	_, report, found := strings.Cut(failureSummaryComment(t, fixture), reportStart)
	if !found {
		t.Fatal("The summary must identify the start of its report.")
	}

	report, _, found = strings.Cut(report, reportEnd)
	if !found {
		t.Fatal("The summary must identify the end of its report.")
	}

	report = strings.TrimSpace(report)
	wantLink := fmt.Sprintf(
		"[Review summary](https://github.com/owner/repo/pull/7#issuecomment-%.0f)",
		summaryID,
	)
	assertPublished := func(t *testing.T) {
		t.Helper()
		body := failureSummaryComment(t, fixture)
		previous := -1

		for _, content := range []string{
			"### Verdict",
			"\n### Summary\n",
			"\n### Changes\n",
			"<summary>Review details</summary>",
		} {
			position := strings.Index(body, content)
			if position <= previous {
				t.Errorf(
					"The summary must display %q in order above its review details:\n%s",
					content,
					body,
				)

				break
			}

			previous = position
		}

		for _, unwanted := range []string{
			finding.Title,
			finding.Body,
			finding.Suggestion,
			titleFinding.Title,
			titleFinding.Body,
			titleFinding.Suggestion,
			inline.Title,
			inline.Body,
		} {
			if strings.Contains(body, unwanted) {
				t.Errorf("The summary must not repeat finding text %q.", unwanted)
			}
		}

		if !strings.Contains(body, "Resolve the findings in the rejection review.") {
			t.Error("The summary verdict must direct the author to the rejection review.")
		}

		if !strings.Contains(body, report) {
			t.Error("The refresh must preserve the complete original report.")
		}

		if len(fixture.state.submittedReviews) != 2 {
			t.Fatal(
				"The refresh must preserve exactly the original inline rejection and one metadata rejection.",
			)
		}

		inlineReview := fixture.state.submittedReviews[0]

		emptyBody, _ := inlineReview["body"].(string)
		if inlineReview["id"] != inlineReviewID || inlineReview["state"] != "CHANGES_REQUESTED" ||
			emptyBody != "" {
			t.Error("The refresh must preserve the original empty inline rejection.")
		}

		native := fixture.state.submittedReviews[1]

		body, ok := native["body"].(string)
		if !ok || native["state"] != "CHANGES_REQUESTED" {
			t.Fatalf("The metadata finding must remain in a native rejection: %v.", native)
		}

		for _, content := range []string{
			"Pull request description, lines 1-1",
			finding.Title,
			finding.Body,
			"```text\n" + finding.Suggestion + "\n```",
			"Pull request title, lines 1-1",
			titleFinding.Title,
			titleFinding.Body,
			"```text\n" + titleFinding.Suggestion + "\n```",
			wantLink,
		} {
			if strings.Count(body, content) != 1 {
				t.Errorf("The native rejection must display %q once:\n%s", content, body)
			}
		}

		if details := strings.Index(body, "<details>"); details >= 0 &&
			strings.Index(body, finding.Suggestion) > details {
			t.Error("The native rejection must expand the exact correction.")
		}

		for _, unwanted := range []string{inline.Title, inline.Body} {
			if strings.Contains(body, unwanted) {
				t.Errorf("The metadata rejection must not repeat inline finding text %q.", unwanted)
			}
		}

		if len(fixture.state.streamedComments) != 1 ||
			fmt.Sprint(fixture.state.streamedComments[0]["body"]) != inlineBody ||
			!strings.Contains(inlineBody, inline.Body) ||
			len(fixture.state.threadNodes) != 1 ||
			fixture.state.threadNodes[0]["id"] != threadID {
			t.Error("The file finding must remain in its original inline thread.")
		}

		if len(fixture.state.issueComments) != 1 ||
			fixture.state.issueComments[0]["id"] != summaryID ||
			native["id"] != nativeID {
			t.Error("The refresh must update the original summary and metadata rejection.")
		}
	}
	t.Run("published", assertPublished)

	for _, scenario := range []string{
		"refresh",
		"legacy section order",
		"missing verdict start",
		"missing verdict end",
	} {
		t.Run(
			scenario,
			func(t *testing.T) {
				body := failureSummaryComment(t, fixture)
				if scenario != "refresh" {
					body = strings.Replace(
						body,
						"<!-- pr-review-agent:verdict-section:end -->",
						finding.Body+"\n\n"+finding.Suggestion+"\n\n<!-- pr-review-agent:verdict-section:end -->",
						1,
					)
				}

				switch scenario {
				case "legacy section order":
					body = strings.ReplaceAll(body, reportStart, "")
					body = strings.ReplaceAll(body, reportEnd, "")
					verdict := strings.Index(body, "<!-- pr-review-agent:verdict-section:start -->")
					summary := strings.Index(body, "\n### Summary\n")

					details := strings.Index(body, "<details>")
					if verdict < 0 || summary < 0 || details < 0 {
						t.Fatal(
							"The saved summary must contain the report, verdict, and review details.",
						)
					}

					if verdict < summary {
						body = body[:verdict] + body[summary:details] + body[verdict:summary] + body[details:]
					}
				case "missing verdict start":
					body = strings.Replace(
						body,
						"<!-- pr-review-agent:verdict-section:start -->",
						"",
						1,
					)
				case "missing verdict end":
					body = strings.Replace(
						body,
						"<!-- pr-review-agent:verdict-section:end -->",
						"",
						1,
					)
				}

				fixture.state.issueComments[0]["body"] = body
				job := fixture.job()

				job.DeliveryID = scenario
				if err := fixture.run(t.Context(), job); err != nil {
					t.Fatal(err)
				}

				assertPublished(t)

				if len(model.prompts) != 1 {
					t.Fatal("The verdict refresh must reuse the completed metadata assessment.")
				}
			},
		)
	}
}

func TestServiceUsesMetadataDisplayAtTheNativeReview(t *testing.T) {
	for _, display := range []reviewrules.FindingDisplay{
		reviewrules.FindingsExpanded,
		reviewrules.FindingsCollapsed,
		reviewrules.FindingsHidden,
	} {
		t.Run(
			string(display),
			func(t *testing.T) {
				finding := metadataPresentationFinding()
				model := &sequenceModel{
					results: []domain.ReviewResult{{Findings: []domain.Finding{finding}}},
				}
				fixture, _ := newContentCollectionFixture(
					t,
					model,
					contentCollectionOptions{
						description:  finding.Evidence,
						onlyMetadata: true,
					},
				)
				policy := loadFindingPolicy(t, display)
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))

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
					policy,
				)
				if err := fixture.run(t.Context(), fixture.job()); err != nil {
					t.Fatal(err)
				}

				if len(
					fixture.state.issueComments,
				) != 1 || len(fixture.state.submittedReviews) != 1 {
					t.Fatal("The display mode must preserve one summary and one rejection.")
				}

				summary := failureSummaryComment(t, fixture)

				stored := retainedMetadataFindings(t, summary)
				if len(stored) != 1 || stored[0].Body != finding.Body ||
					stored[0].CorrectionAction != finding.CorrectionAction ||
					stored[0].Suggestion != finding.Suggestion {
					t.Fatal(
						"Every display mode must retain the complete finding and exact correction in the summary checkpoint.",
					)
				}

				for _, content := range []string{finding.Title, finding.Body, finding.Suggestion} {
					if strings.Contains(summary, content) {
						t.Fatalf(
							"The summary must not repeat %q from the metadata rejection.",
							content,
						)
					}
				}

				native := fixture.state.submittedReviews[0]

				body, ok := native["body"].(string)
				if !ok || native["state"] != "CHANGES_REQUESTED" ||
					!strings.Contains(
						body,
						"[Review summary](https://github.com/owner/repo/pull/7#issuecomment-2000)",
					) {
					t.Fatalf(
						"Every display mode must preserve the rejection and summary backlink: %v.",
						native,
					)
				}

				for _, content := range []string{finding.Title, finding.Body, finding.Suggestion} {
					position := strings.Index(body, content)

					switch display {
					case reviewrules.FindingsExpanded:
						if position < 0 || strings.Count(body, content) != 1 ||
							strings.Contains(body, "<details>") {
							t.Fatalf("The native review must expand %q once.", content)
						}
					case reviewrules.FindingsCollapsed:
						start, end := strings.Index(body, "<details>"), strings.Index(
							body,
							"</details>",
						)
						if position < 0 || start < 0 || position < start || end < position ||
							strings.Count(body, content) != 1 {
							t.Fatalf(
								"The native review must include %q once inside its closed details section.",
								content,
							)
						}
					case reviewrules.FindingsHidden:
						if position >= 0 {
							t.Fatalf(
								"The hidden display mode must omit %q from the native review.",
								content,
							)
						}
					}
				}
			},
		)
	}
}

func TestServicePublishesMetadataWithoutAnExactCorrection(t *testing.T) {
	finding := metadataPresentationFinding()
	finding.CorrectionAction = domain.CorrectionNone
	finding.Suggestion = ""
	model := &sequenceModel{results: []domain.ReviewResult{{Findings: []domain.Finding{finding}}}}

	fixture, _ := newContentCollectionFixture(
		t,
		model,
		contentCollectionOptions{
			description:  finding.Evidence,
			onlyMetadata: true,
		},
	)
	if err := fixture.run(t.Context(), fixture.job()); err != nil {
		t.Fatal(err)
	}

	if len(fixture.state.submittedReviews) != 1 || len(fixture.state.issueComments) != 1 {
		t.Fatal("Metadata without an exact correction must publish one rejection and one summary.")
	}

	native := fixture.state.submittedReviews[0]

	body, ok := native["body"].(string)
	if !ok || native["state"] != "CHANGES_REQUESTED" || strings.Count(body, finding.Title) != 1 ||
		strings.Count(body, finding.Body) != 1 {
		t.Fatalf(
			"The native rejection must explain the complete metadata finding without requiring an exact correction: %v.",
			native,
		)
	}

	for _, unwanted := range []string{"Replace those lines with:", "Delete lines", "```text"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("The native rejection must not invent an unavailable correction: %s", body)
		}
	}

	summary := failureSummaryComment(t, fixture)
	if strings.Contains(summary, finding.Title) || strings.Contains(summary, finding.Body) {
		t.Fatal("The summary must not repeat metadata feedback from the rejection review.")
	}
}

func TestMetadataPublicationRetryReusesTheSavedAssessment(t *testing.T) {
	for _, operation := range []string{"submit", "update", "empty rejection submit"} {
		t.Run(
			operation,
			func(t *testing.T) {
				finding := metadataPresentationFinding()
				prior := reassessmentFinding()
				model := &sequenceModel{
					results: []domain.ReviewResult{
						{Findings: []domain.Finding{finding}},
					},
					metadataAssessments: [][]domain.MetadataFindingAssessment{
						metadataDecision("1", domain.ResolutionResolved, finding.Evidence),
					},
				}
				fixture, _ := newContentCollectionFixture(
					t,
					model,
					contentCollectionOptions{
						description:  finding.Evidence,
						onlyMetadata: true,
					},
				)
				seedMetadataReassessment(t, fixture, []domain.Finding{prior}, true)
				summaryID := fixture.state.issueComments[0]["id"]
				job := fixture.job()

				job.MetadataOnly = true
				if operation != "submit" {
					body := marker.Review(job.Head, domain.ReviewDecisionRequestChanges)
					if operation == "empty rejection submit" {
						body = ""
					}

					fixture.state.reviewPages = [][]map[string]any{
						{
							{
								"id":                         float64(4100),
								collectionFixtureCommitIDKey: string(fixture.job().Head),
								collectionFixtureStateKey:    collectionFixtureRejectionState,

								"body": body,
								"user": map[string]any{"login": testBotLogin},
							},
						},
					}
				}

				if operation == "update" {
					fixture.state.updateReviewStatus = http.StatusInternalServerError
				} else {
					fixture.state.submitReviewStatus = http.StatusInternalServerError
				}

				if err := fixture.run(t.Context(), job); err == nil {
					t.Fatal("The review must report the native publication failure.")
				}

				body := failureSummaryComment(t, fixture)

				stored := retainedMetadataFindings(t, body)
				if len(stored) != 1 || stored[0].Body != finding.Body ||
					stored[0].Suggestion != finding.Suggestion ||
					len(model.prompts) != 1 ||
					len(model.metadataPrompts) != 1 {
					t.Fatalf(
						"The failed publication must save the newly assessed finding and correction: %+v.",
						stored,
					)
				}

				pullRequest, err := fixture.github.GetPullRequest(
					t.Context(),
					job.InstallationID,
					job.Repository,
					job.Number,
				)
				if err != nil {
					t.Fatal(err)
				}

				if !review.MetadataMatches(body, job.Head, diff.PullRequestRevision(pullRequest)) ||
					len(fixture.state.issueComments) != 1 ||
					fixture.state.issueComments[0]["id"] != summaryID ||
					fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
					t.Fatal(
						"The failed publication must preserve the current assessment in the original summary and fail its check.",
					)
				}

				fixture.state.updateReviewStatus = http.StatusOK
				fixture.state.submitReviewStatus = http.StatusOK

				job.DeliveryID = "native-publication-recovered"
				if err := fixture.run(t.Context(), job); err != nil {
					t.Fatal(err)
				}

				if len(model.prompts) != 1 || len(model.metadataPrompts) != 1 ||
					len(fixture.state.issueComments) != 1 ||
					fixture.state.issueComments[0]["id"] != summaryID {
					t.Fatal(
						"The publication retry must reuse the saved assessment and original summary without another model request.",
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

				metadataReviewCount := 0

				var metadataReviewID int64

				for _, native := range reviews {
					if native.State == "CHANGES_REQUESTED" &&
						strings.Contains(native.Body, finding.Body) &&
						strings.Contains(native.Body, finding.Suggestion) &&
						!strings.Contains(native.Body, prior.Body) {
						metadataReviewCount++
						metadataReviewID = native.ID
					}
				}

				if metadataReviewCount != 1 {
					t.Fatal(
						"The publication retry must publish the newly assessed finding and exact correction in one metadata rejection.",
					)
				}

				if operation == "empty rejection submit" &&
					(len(reviews) != 2 || fixture.state.reviewPages[0][0]["id"] != float64(
						4100,
					) || fixture.state.reviewPages[0][0]["body"] != "") {
					t.Fatal(
						"The publication retry must retain the original empty rejection alongside one metadata rejection.",
					)
				}

				job.DeliveryID = "native-publication-repeat"
				if err := fixture.run(t.Context(), job); err != nil {
					t.Fatal(err)
				}

				repeated, err := fixture.github.ListReviews(
					t.Context(),
					job.InstallationID,
					job.Repository,
					job.Number,
				)
				if err != nil {
					t.Fatal(err)
				}

				if len(repeated) != len(reviews) || len(model.prompts) != 1 ||
					len(model.metadataPrompts) != 1 {
					t.Fatal(
						"The repeated refresh must preserve the reviews and cached model assessment.",
					)
				}

				for _, native := range repeated {
					if strings.Contains(
						native.Body,
						finding.Body,
					) && native.ID != metadataReviewID {
						t.Fatal("The repeated refresh must update the existing metadata rejection.")
					}
				}
			},
		)
	}
}

func TestCrossHeadMetadataProjectionPreservesHistoricalReviews(t *testing.T) {
	for _, scenario := range []string{
		"unchanged",
		"changed",
		"legacy summary link",
		"publication retry",
	} {
		t.Run(
			scenario,
			func(t *testing.T) {
				finding := metadataPresentationFinding()
				current := finding.Evidence
				model := &sequenceModel{
					results: []domain.ReviewResult{{Findings: []domain.Finding{finding}}},
				}
				fixture, _ := newContentCollectionFixture(
					t,
					model,
					contentCollectionOptions{
						onlyMetadata:       true,
						currentDescription: func() string { return current },
					},
				)

				job := fixture.job()
				if err := fixture.run(t.Context(), job); err != nil {
					t.Fatal(err)
				}

				if len(
					fixture.state.submittedReviews,
				) != 1 || len(fixture.state.issueComments) != 1 {
					t.Fatal("The fixture must begin with one metadata rejection and one summary.")
				}

				summaryID := fixture.state.issueComments[0]["id"]
				original := fixture.state.submittedReviews[0]
				originalID := original["id"]

				body, ok := original["body"].(string)
				if !ok {
					t.Fatal("The original metadata rejection must contain its full body.")
				}

				visible, _, found := strings.Cut(
					body,
					marker.Review(job.Head, domain.ReviewDecisionRequestChanges),
				)
				if !found {
					t.Fatal("The original metadata rejection must retain its review checkpoint.")
				}

				pullRequest, err := fixture.github.GetPullRequest(
					t.Context(),
					job.InstallationID,
					job.Repository,
					job.Number,
				)
				if err != nil {
					t.Fatal(err)
				}

				oldSummary := review.Summary{
					Head:             domain.HeadSHA(testStaleHeadSHA),
					Decision:         domain.ReviewDecisionRequestChanges,
					CoverageComplete: true,
					Metadata:         []domain.Finding{finding},
					MetadataRevision: diff.PullRequestRevision(pullRequest),
				}

				if scenario == "legacy summary link" {
					visible = fmt.Sprintf(
						"[Review summary](https://github.com/owner/repo/pull/7#issuecomment-%.0f)\n\n",
						summaryID,
					)
				}

				originalBody := visible + review.RenderVerdictBody(oldSummary)
				original["commit_id"] = testStaleHeadSHA
				original["body"] = originalBody

				if scenario == "changed" || scenario == "publication retry" {
					current = "Changed summary fragment"
					finding.Title = "Explain the changed review projection"
					finding.Body = "The revised description omits a complete explanation. State the changed review behavior in a complete sentence."
					finding.Evidence = current
					finding.Suggestion = "The rejection review displays the current metadata findings and corrections."
					model.results = append(
						model.results,
						domain.ReviewResult{Findings: []domain.Finding{finding}},
					)
					model.metadataAssessments = [][]domain.MetadataFindingAssessment{
						metadataDecision("1", domain.ResolutionResolved, current),
					}
				}

				job.DeliveryID = "cross-head-projection"
				job.MetadataOnly = true

				if scenario == "publication retry" {
					fixture.state.submitReviewStatus = http.StatusInternalServerError
					if err := fixture.run(t.Context(), job); err == nil {
						t.Fatal(
							"The cross-head metadata submission must report the GitHub failure.",
						)
					}

					stored := retainedMetadataFindings(t, failureSummaryComment(t, fixture))
					if len(
						stored,
					) != 1 || stored[0].Body != finding.Body || len(model.prompts) != 2 ||
						len(model.metadataPrompts) != 1 ||
						len(fixture.state.submittedReviews) != 1 ||
						original["body"] != originalBody {
						t.Fatal(
							"The failed cross-head submission must cache the current assessment and preserve the historical review.",
						)
					}

					fixture.state.submitReviewStatus = http.StatusOK
					job.DeliveryID = "cross-head-publication-recovered"
				}

				if err := fixture.run(t.Context(), job); err != nil {
					t.Fatal(err)
				}

				expectedFreshCalls, expectedReassessmentCalls := 1, 0
				if scenario == "changed" || scenario == "publication retry" {
					expectedFreshCalls, expectedReassessmentCalls = 2, 1
				}

				if len(model.prompts) != expectedFreshCalls ||
					len(model.metadataPrompts) != expectedReassessmentCalls {
					t.Fatal(
						"The cross-head publication must reuse every completed fresh review and reassessment.",
					)
				}

				expectedReviews := 2
				if scenario == "unchanged" {
					expectedReviews = 1
				}

				assertPublished := func(t *testing.T) {
					t.Helper()

					if original["id"] != originalID || original["commit_id"] != testStaleHeadSHA ||
						original["body"] != originalBody {
						t.Fatal(
							"The current-head projection must not overwrite the historical review or its old-head checkpoint.",
						)
					}

					if len(fixture.state.submittedReviews) != expectedReviews ||
						len(fixture.state.issueComments) != 1 ||
						fixture.state.issueComments[0]["id"] != summaryID {
						t.Fatal(
							"The projection must preserve one summary and publish a current-head rejection only when its visible content changes.",
						)
					}

					if scenario != "unchanged" {
						native := fixture.state.submittedReviews[1]
						assertGitReviewProjection(t, native, job.Head, finding)
					}
				}
				assertPublished(t)

				freshCalls, reassessmentCalls := len(model.prompts), len(model.metadataPrompts)

				var currentReviewID any
				if scenario != "unchanged" {
					currentReviewID = fixture.state.submittedReviews[1]["id"]
				}

				job.DeliveryID = "cross-head-projection-repeat"
				if err := fixture.run(t.Context(), job); err != nil {
					t.Fatal(err)
				}

				assertPublished(t)

				if len(model.prompts) != freshCalls ||
					len(model.metadataPrompts) != reassessmentCalls ||
					scenario != "unchanged" &&
						fixture.state.submittedReviews[1]["id"] != currentReviewID {
					t.Fatal(
						"The next refresh must preserve the rejection and reuse the completed model assessment.",
					)
				}
			},
		)
	}
}

package review_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestReconciliationFailurePreservesSavedFindingCheckpoints(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{reconcileErr: errors.New("GitHub thread reconciliation failed")})
	job := fixture.forcedJob()
	pr, err := fixture.github.GetPullRequest(t.Context(), job.InstallationID, job.Repository, job.Number)
	if err != nil {
		t.Fatal(err)
	}
	summary := findingDetailsSummary()
	summary.MetadataRevision = diff.PullRequestRevision(pr)
	summary.Fallback = []domain.Finding{severeFinding()}
	original := review.RenderBody(summary)
	fixture.state.issueComments = []map[string]any{{
		"id": float64(2000), "user": map[string]any{"login": testBotLogin},
		"body": original + "\n" + marker.EncodeState(marker.State{RunID: "saved-review", Status: marker.StateReviewing}),
	}}
	if err := fixture.run(t.Context(), job); err == nil {
		t.Fatal("The review must report the failed thread reconciliation.")
	}
	body := failureSummaryComment(t, fixture)
	if strings.Count(body, summary.Metadata[0].Body) != 1 || strings.Count(body, summary.Fallback[0].Body) != 1 || !review.MetadataMatches(body, job.Head, summary.MetadataRevision) {
		t.Fatal("The reconciliation failure erased saved metadata or refused findings.")
	}
	const prefix = "<!-- pr-review-agent:fallback-record:v1 "
	_, remaining, _ := strings.Cut(original, prefix)
	payload, _, _ := strings.Cut(remaining, " -->")
	if !strings.Contains(body, prefix+payload+" -->") {
		t.Fatal("The reconciliation failure changed the saved refused finding checkpoint.")
	}
	if _, complete := marker.FindReview(body); complete || len(fixture.state.submittedReviews) != 0 || len(fixture.state.issueComments) != 1 {
		t.Fatal("The failed reconciliation must preserve one incomplete summary without submitting a verdict.")
	}
}

func TestPublicationFailurePreservesAnEarlierRefusedFinding(t *testing.T) {
	finding := severeFinding()
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model:                           &sequenceModel{results: []domain.ReviewResult{{Findings: []domain.Finding{finding}}}},
		createCommentStatus:             http.StatusUnprocessableEntity,
		pullRequestStatusAfterFirstRead: http.StatusBadGateway,
	})
	if err := fixture.run(t.Context(), fixture.job()); err == nil {
		t.Fatal("The publication refresh must report the failed GitHub read.")
	}
	body := failureSummaryComment(t, fixture)
	if strings.Count(body, finding.Body) != 1 || !strings.Contains(body, "<!-- pr-review-agent:fallback-record:v1 ") {
		t.Fatal("The publication failure erased the earlier refused inline finding or its checkpoint.")
	}
	if _, completed := marker.FindReview(body); completed || len(fixture.state.submittedReviews) != 0 || len(fixture.state.issueComments) != 1 {
		t.Fatal("The failed publication must preserve one incomplete summary without submitting a verdict.")
	}
	const prefix = "<!-- pr-review-agent:fallback-record:v1 "
	_, remaining, _ := strings.Cut(body, prefix)
	payload, _, _ := strings.Cut(remaining, " -->")
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Findings   []domain.Finding `json:"findings"`
		Checkpoint struct {
			Prior []int `json:"prior"`
		} `json:"checkpoint"`
	}
	if err := json.Unmarshal(decoded, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Findings) != 1 || record.Findings[0].Body != finding.Body || len(record.Checkpoint.Prior) != 0 {
		t.Fatal("The failure checkpoint must retain the current refused finding and its provenance.")
	}
}

func TestResumedReviewSeparatesPriorFindingsFromCompletedResults(t *testing.T) {
	for _, scenario := range []string{"clean fragment", "metadata finding", "legacy metadata finding", "refused inline finding"} {
		t.Run(scenario, func(t *testing.T) {
			firstLine := "First fragment " + strings.Repeat("a", 100000)
			secondLine := "Second fragment " + strings.Repeat("b", 100000)
			firstResult := domain.ReviewResult{}
			if strings.Contains(scenario, "metadata finding") {
				firstResult.Findings = []domain.Finding{{
					Surface: domain.FindingPullRequestDescription, RuleID: "complete_sentences",
					StartLine: 1, EndLine: 1, Title: "Complete the first explanation",
					Body: "The first description fragment requires a complete sentence.", Evidence: firstLine, Importance: 10,
				}}
			}
			if scenario == "refused inline finding" {
				firstResult.Findings = []domain.Finding{{
					Path: "main.go", StartLine: 2, EndLine: 2, Title: "Validate the added value",
					Body: "The added value requires validation before use.", Evidence: "added", Importance: 10,
				}}
			}
			model := &sequenceModel{results: []domain.ReviewResult{firstResult}, err: &openai.TruncatedError{Model: testReviewModel}, healthyPrompt: "Current prose, lines 1-2:"}
			if scenario == "refused inline finding" {
				model.healthyPrompt = "File: main.go\nStatus: modified\n"
			}
			fixture, _ := newContentCollectionFixture(t, model, contentCollectionOptions{description: firstLine + "\n\n" + secondLine, onlyMetadata: scenario != "refused inline finding"})
			if scenario == "refused inline finding" {
				fixture.state.createCommentStatus = http.StatusUnprocessableEntity
			}
			job := fixture.forcedJob()
			job.Settings.ChunkConcurrency = 1
			pr, err := fixture.github.GetPullRequest(t.Context(), job.InstallationID, job.Repository, job.Number)
			if err != nil {
				t.Fatal(err)
			}
			summary := findingDetailsSummary()
			summary.MetadataRevision = diff.PullRequestRevision(pr)
			summary.Metadata[0].Surface = domain.FindingPullRequestDescription
			summary.Metadata[0].RuleID = "complete_sentences"
			summary.Metadata[0].Body = "The previous assessment reported a description defect that the current review must reassess."
			priorBody := summary.Metadata[0].Body
			if scenario == "legacy metadata finding" {
				summary.Metadata = nil
				priorBody = ""
			}
			fixture.state.issueComments = []map[string]any{{
				"id": float64(2000), "user": map[string]any{"login": testBotLogin},
				"body": review.RenderBody(summary) + "\n" + marker.EncodeState(marker.State{
					RunID: "prior-assessment", Status: marker.StateReviewing,
				}),
			}}
			if err := fixture.run(t.Context(), job); err != nil {
				t.Fatal(err)
			}
			state := decodedSummaryState(t, fixture)
			if len(state.Completed) != 1 || len(state.Pending) == 0 || scenario != "refused inline finding" && len(state.Pending) != 1 {
				t.Fatalf("The first attempt must finish A and leave B pending: %+v.", state)
			}
			if body := failureSummaryComment(t, fixture); priorBody != "" && !strings.Contains(body, priorBody) {
				t.Fatal("The partial field assessment lost its prior finding before B completed.")
			}
			if strings.Contains(scenario, "metadata finding") && !strings.Contains(failureSummaryComment(t, fixture), firstResult.Findings[0].Body) {
				t.Fatalf("The first attempt did not publish A's finding: %+v.", retainedMetadataFindings(t, failureSummaryComment(t, fixture)))
			}
			if scenario == "legacy metadata finding" {
				summary.Metadata = retainedMetadataFindings(t, failureSummaryComment(t, fixture))
				summary.CoverageComplete = false
				fixture.state.issueComments[0]["body"] = review.RenderIncompleteBody(summary, len(state.Pending), "The provider request failed.", "") + "\n" + marker.EncodeState(state)
			}
			firstCalls := len(model.prompts)
			model.err = nil
			for range state.Pending {
				model.results = append(model.results, domain.ReviewResult{})
			}
			job.Forced = false
			job.DeliveryID = "resumed-metadata-assessment"
			admitted, accepted, err := fixture.service.Admit(t.Context(), job)
			if err != nil || !accepted {
				t.Fatalf("The resumed review did not admit B: %v.", err)
			}
			outcome, err := fixture.service.RunWithOutcome(t.Context(), admitted)
			if err != nil || !outcome.CoverageComplete || outcome.Disposition == domain.AssessmentIncomplete || outcome.Disposition == domain.AssessmentFailed {
				t.Fatalf("The resumed review did not complete B: %+v, %v.", outcome, err)
			}
			if len(model.prompts) != firstCalls+len(state.Pending) {
				t.Fatal("The resumed review repeated A instead of reviewing only B.")
			}
			for _, prompt := range model.prompts[firstCalls:] {
				if strings.Contains(prompt, model.healthyPrompt) {
					t.Fatal("The resumed review included A's completed source fragment.")
				}
			}
			body := failureSummaryComment(t, fixture)
			if priorBody != "" && strings.Contains(body, priorBody) {
				t.Fatal("The completed field retained a finding from the prior assessment.")
			}
			findings := retainedMetadataFindings(t, body)
			if strings.Contains(scenario, "metadata finding") {
				if len(findings) != 1 || findings[0].Body != firstResult.Findings[0].Body {
					t.Fatal("The resumed review lost the new finding returned by A.")
				}
			} else if len(findings) != 0 {
				t.Fatal("The completed clean metadata retained an obsolete finding.")
			}
			if scenario == "refused inline finding" {
				if strings.Count(body, firstResult.Findings[0].Body) != 1 || !strings.Contains(body, "<!-- pr-review-agent:fallback-record:v1 ") {
					t.Fatal("The resumed review lost A's refused inline finding.")
				}
				job.Forced = true
				job.DeliveryID = "fresh-fallback-reassessment"
				model.results = append(model.results, make([]domain.ReviewResult, len(state.Completed)+len(state.Pending))...)
				if err := fixture.run(t.Context(), job); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(failureSummaryComment(t, fixture), firstResult.Findings[0].Body) || strings.Contains(failureSummaryComment(t, fixture), "<!-- pr-review-agent:fallback-record:v1 ") {
					t.Fatal("The complete fresh reassessment retained the old refused finding.")
				}
			}
			if len(fixture.state.issueComments) != 1 || fixture.state.issueComments[0]["id"] != float64(2000) {
				t.Fatal("The resumed review replaced the original summary comment.")
			}
		})
	}
}

func TestForcedIncompleteReviewRetainsOnlyCurrentMetadataUntilReassessment(t *testing.T) {
	for _, scenario := range []string{"clear after reassessment", "replace after reassessment", "stale head", "stale revision"} {
		t.Run(scenario, func(t *testing.T) {
			model := &sequenceModel{err: &openai.TruncatedError{Model: testReviewModel}}
			fixture, _ := newContentCollectionFixture(t, model)
			job := fixture.forcedJob()
			pr, err := fixture.github.GetPullRequest(t.Context(), job.InstallationID, job.Repository, job.Number)
			if err != nil {
				t.Fatal(err)
			}
			summary := findingDetailsSummary()
			summary.MetadataRevision = diff.PullRequestRevision(pr)
			summary.Metadata[0].RuleID = "pr_title"
			summary.Metadata[0].Body = strings.Repeat("The title omits the changed review behavior that distinguishes this fixture from other review tests. ", 4)
			summary.Metadata[0].Body = strings.TrimSpace(summary.Metadata[0].Body)
			saved := summary.Metadata[0]
			summary.Fallback = []domain.Finding{{
				Path: "main.go", StartLine: 2, EndLine: 2,
				Title: "Preserve the saved refusal", Body: "The previous assessment could not publish this file finding inline.", Importance: 10,
			}}
			fallbackBody := summary.Fallback[0].Body
			switch scenario {
			case "stale head":
				summary.Head = domain.HeadSHA(testStaleHeadSHA)
			case "stale revision":
				summary.MetadataRevision = "previous-metadata-revision"
			}
			fixture.state.issueComments = []map[string]any{{
				"id": float64(2000), "user": map[string]any{"login": testBotLogin},
				"body": review.RenderBody(summary) + "\n" + marker.EncodeState(marker.State{
					RunID: "previous-partial-review", Status: marker.StateReviewing,
					Pending: []string{"pending-metadata"},
				}),
			}}
			fixture.state.reviewPages = [][]map[string]any{{{
				"id": float64(4100), "commit_id": testHeadSHA, "state": "DISMISSED",
				"body": marker.Review(job.Head, domain.ReviewDecisionRequestChanges),
				"user": map[string]any{"login": testBotLogin},
			}}}
			admitted, accepted, err := fixture.service.Admit(t.Context(), job)
			if err != nil || !accepted {
				t.Fatalf("The forced review did not admit the current fixture: %v.", err)
			}
			outcome, err := fixture.service.RunWithOutcome(t.Context(), admitted)
			if err != nil || outcome.Disposition != domain.AssessmentIncomplete || outcome.CoverageComplete {
				t.Fatalf("The truncated response did not produce an incomplete assessment: %+v, %v.", outcome, err)
			}
			body := failureSummaryComment(t, fixture)
			state := decodedSummaryState(t, fixture)
			if state.LastReviewed != "" || state.Status == marker.StateDone || len(state.Pending)+len(state.Unread) == 0 {
				t.Fatalf("The truncated review completed unread metadata: %+v.", state)
			}
			if _, complete := marker.FindReview(body); complete || len(fixture.state.submittedReviews) != 0 || len(fixture.state.dismissals) != 0 {
				t.Fatal("The incomplete review published a completion marker or changed the dismissed verdict.")
			}
			if len(fixture.state.issueComments) != 1 || fixture.state.issueComments[0]["id"] != float64(2000) {
				t.Fatal("The incomplete review replaced the original summary comment.")
			}
			stored := retainedMetadataFindings(t, body)
			if strings.HasPrefix(scenario, "stale") {
				if len(stored) != 0 || strings.Contains(body, saved.Body) || strings.Contains(body, fallbackBody) {
					t.Fatal("The incomplete review published stale findings as current metadata.")
				}
				return
			}
			if len(stored) != 1 || stored[0].Body != saved.Body || strings.Count(body, saved.Body) != 1 || !review.MetadataMatches(body, job.Head, diff.PullRequestRevision(pr)) {
				t.Fatal("The incomplete review lost the complete saved finding or its current context.")
			}
			if strings.Count(body, fallbackBody) != 1 {
				t.Fatal("The incomplete review lost its saved refused inline finding.")
			}
			model.err = nil
			result := domain.ReviewResult{}
			if scenario == "replace after reassessment" {
				replacement := saved
				replacement.Body = "The title does not identify summary link validation. State that behavior in the title."
				replacement.Evidence = pr.Title
				result.Findings = []domain.Finding{replacement}
			}
			model.results = []domain.ReviewResult{result}
			job.DeliveryID = "forced-metadata-reassessment"
			admitted, accepted, err = fixture.service.Admit(t.Context(), job)
			if err != nil || !accepted {
				t.Fatalf("The successful reassessment did not admit the fixture: %v.", err)
			}
			outcome, err = fixture.service.RunWithOutcome(t.Context(), admitted)
			if err != nil || outcome.Disposition != domain.AssessmentCompleted || !outcome.CoverageComplete {
				t.Fatalf("The successful reassessment did not complete: %+v, %v.", outcome, err)
			}
			body = failureSummaryComment(t, fixture)
			stored = retainedMetadataFindings(t, body)
			if strings.Contains(body, saved.Body) || strings.Contains(body, fallbackBody) || len(stored) != len(result.Findings) {
				t.Fatal("The successful reassessment retained an old finding instead of its current result.")
			}
			if len(stored) > 0 && stored[0].Body != result.Findings[0].Body {
				t.Fatal("The reassessment did not replace the old explanation with the new finding.")
			}
			if len(fixture.state.issueComments) != 1 {
				t.Fatal("The reassessment created another summary comment.")
			}
		})
	}
}

func retainedMetadataFindings(t *testing.T, body string) []domain.Finding {
	t.Helper()
	const prefix = "<!-- pr-review-agent:metadata:v1 "
	start := strings.LastIndex(body, prefix)
	if start < 0 {
		t.Fatal("The summary did not retain a metadata checkpoint.")
	}
	payload, _, found := strings.Cut(body[start+len(prefix):], " -->")
	if !found {
		t.Fatal("The metadata checkpoint is incomplete.")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Findings []domain.Finding `json:"findings"`
	}
	if err := json.Unmarshal(decoded, &record); err != nil {
		t.Fatal(err)
	}
	return record.Findings
}

func TestUnreadableSummaryPreservesFindingCheckpointsWithoutCompletingTheReview(t *testing.T) {
	summary := findingDetailsSummary()
	summary.Fallback = []domain.Finding{{
		Path: "main.go", StartLine: 2, EndLine: 2, Title: "Validate the changed value",
		Body: "The changed value requires validation before the write.", Importance: 10,
	}}
	body := review.RenderUnreadableBody(summary, "The model did not finish reviewing the current changes.")
	if _, completed := marker.FindReview(body); completed {
		t.Fatal("The unreadable summary must not claim that the head completed review.")
	}
	stored := retainedMetadataFindings(t, body)
	if len(stored) != 1 || stored[0].Body != summary.Metadata[0].Body || strings.Count(body, stored[0].Body) != 1 {
		t.Fatal("The unreadable summary must display and persist its metadata finding.")
	}
	const prefix = "<!-- pr-review-agent:fallback-record:v1 "
	_, checkpoint, found := strings.Cut(body, prefix)
	if !found {
		t.Fatal("The unreadable summary must persist its refused inline finding.")
	}
	payload, _, found := strings.Cut(checkpoint, " -->")
	if !found {
		t.Fatal("The refused inline finding checkpoint is incomplete.")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Findings []domain.Finding `json:"findings"`
	}
	if err := json.Unmarshal(decoded, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Findings) != 1 || record.Findings[0].Body != summary.Fallback[0].Body || strings.Count(body, summary.Fallback[0].Body) != 1 || !strings.Contains(body, "The review submitted no verdict.") {
		t.Fatal("The unreadable summary must retain its refused finding and neutral verdict.")
	}
}

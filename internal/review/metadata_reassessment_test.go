package review_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

func (model *sequenceModel) ReassessMetadata(_ context.Context, request review.MetadataReassessmentRequest) ([]domain.MetadataFindingAssessment, error) {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.metadataPrompts = append(model.metadataPrompts, request.Prompt)
	if model.metadataHook != nil {
		model.metadataHook()
	}
	if model.metadataError != nil {
		return nil, model.metadataError
	}
	index := len(model.metadataPrompts) - 1
	if index >= len(model.metadataAssessments) {
		return nil, errors.New("unexpected metadata reassessment")
	}
	return model.metadataAssessments[index], nil
}

func metadataDecision(identifier string, resolution domain.Resolution, evidence string) []domain.MetadataFindingAssessment {
	return []domain.MetadataFindingAssessment{{FindingID: identifier, Resolution: resolution, Reason: "The current field establishes this decision.", Evidence: evidence, StartLine: 1, EndLine: 1}}
}

func seedMetadataReassessment(t *testing.T, fixture *serviceFixture, findings []domain.Finding, completed bool) {
	t.Helper()
	job := fixture.job()
	pullRequest, err := fixture.github.GetPullRequest(t.Context(), job.InstallationID, job.Repository, job.Number)
	if err != nil {
		t.Fatal(err)
	}
	summary := testSummary()
	summary.Head = job.Head
	summary.Decision = domain.ReviewDecisionRequestChanges
	summary.Metadata = findings
	summary.MetadataRevision = "previous-metadata-revision"
	state := marker.State{RunID: "previous-review", Status: marker.StateDone, LastReviewed: job.Head}
	if !completed {
		state.LastReviewed = ""
		state.Status = marker.StateReviewing
	}
	fixture.state.issueComments = []map[string]any{{"id": float64(2000), "user": map[string]any{"login": testBotLogin}, "body": review.RenderBody(summary) + "\n" + marker.EncodeState(state)}}
	if diff.PullRequestRevision(pullRequest) == summary.MetadataRevision {
		t.Fatal("The fixture must change the metadata revision.")
	}
}

func reassessmentFinding() domain.Finding {
	return domain.Finding{Surface: domain.FindingPullRequestDescription, RuleID: "complete_sentences", StartLine: 1, EndLine: 1, Title: "Explain the observable repair", Body: "The description lists internal calls instead of explaining the repair behavior.", Importance: 10}
}

func TestMetadataEditRequiresExplicitResolutionOfEachPriorFinding(t *testing.T) {
	for _, mode := range []string{"metadata edit", "ordinary", "forced"} {
		for _, outcome := range []string{"open", "resolved", "invalid original", "uncertain", "partial", "missing", "duplicate", "unknown", "invalid", "ungrounded", "provider failure"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				current := "Timestamp repair calls SharedStore.UpdateRowScalars."
				if outcome == "resolved" {
					current = "Timestamp repair updates existing records without rebuilding the search index."
				}
				model := &sequenceModel{results: []domain.ReviewResult{{}}}
				fixture, _ := newContentCollectionFixture(t, model, contentCollectionOptions{description: current, onlyMetadata: true})
				findings := []domain.Finding{reassessmentFinding()}
				resolution := domain.Resolution(outcome)
				approvalExpected := outcome == "resolved" || outcome == "invalid original"
				if outcome != "resolved" && outcome != "uncertain" {
					resolution = domain.ResolutionOpen
				}
				if outcome == "invalid original" {
					resolution = domain.ResolutionResolved
				}
				model.metadataAssessments = [][]domain.MetadataFindingAssessment{metadataDecision("1", resolution, current)}
				if outcome == "invalid original" {
					model.metadataAssessments[0][0].Reason = "The current description contains a complete sentence, which disproves the original sentence-completeness finding."
				}
				expectError := false
				switch outcome {
				case "partial":
					second := findings[0]
					second.Title = "Explain the affected records"
					second.Body = "The description does not explain which records the repair updates."
					findings = append(findings, second)
					model.metadataAssessments = [][]domain.MetadataFindingAssessment{metadataDecision("1", domain.ResolutionResolved, current), metadataDecision("2", domain.ResolutionOpen, current)}
				case "missing":
					model.metadataAssessments[0] = nil
					expectError = true
				case "duplicate":
					model.metadataAssessments[0] = append(model.metadataAssessments[0], model.metadataAssessments[0][0])
					expectError = true
				case "unknown":
					model.metadataAssessments[0][0].FindingID = "other"
					expectError = true
				case "invalid":
					model.metadataAssessments[0][0].Resolution = "invalid"
					expectError = true
				case "ungrounded":
					model.metadataAssessments[0][0].Evidence = "Unobserved source"
					expectError = true
				case "provider failure":
					model.metadataError = errors.New("metadata provider failed")
					expectError = true
				}
				seedMetadataReassessment(t, fixture, findings, mode == "metadata edit")
				job := fixture.job()
				job.MetadataOnly = mode == "metadata edit"
				job.Forced = mode == "forced"
				err := fixture.run(t.Context(), job)
				if (err != nil) != expectError {
					t.Fatalf("The review error was %v; expected failure is %t.", err, expectError)
				}
				for _, submitted := range fixture.state.submittedReviews {
					if submitted["state"] == "APPROVED" && !approvalExpected {
						t.Fatal("The review approved without resolving every prior finding.")
					}
				}
				if len(model.metadataPrompts) == 0 || !strings.Contains(model.metadataPrompts[0], findings[0].Body) || !strings.Contains(model.metadataPrompts[0], current) {
					t.Fatal("The reassessment omitted the prior objection or complete current field.")
				}
				body := failureSummaryComment(t, fixture)
				if approvalExpected {
					if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) || len(retainedMetadataFindings(t, body)) != 0 {
						t.Fatal("Explicit resolution did not permit approval.")
					}
				} else if outcome == "partial" {
					stored := retainedMetadataFindings(t, body)
					if len(stored) != 1 || stored[0].Title != findings[1].Title || stored[0].Body != model.metadataAssessments[1][0].Reason {
						t.Fatal("The partial resolution erased the unresolved finding.")
					}
				} else {
					stored := retainedMetadataFindings(t, body)
					explanation := findings[0].Body
					if !expectError {
						explanation = model.metadataAssessments[0][0].Reason
					}
					if len(stored) != 1 || stored[0].Title != findings[0].Title || stored[0].Body != explanation {
						t.Fatal("The review erased the prior finding.")
					}
				}
			})
		}
	}
}

func TestMetadataReassessmentRejectsConcurrentDescriptionEdits(t *testing.T) {
	for _, mode := range []string{"metadata edit", "ordinary", "forced"} {
		t.Run(mode, func(t *testing.T) {
			current := "Timestamp repair updates existing records without rebuilding the search index."
			model := &sequenceModel{results: []domain.ReviewResult{{}}, metadataAssessments: [][]domain.MetadataFindingAssessment{metadataDecision("1", domain.ResolutionResolved, current)}}
			fixture, _ := newContentCollectionFixture(t, model, contentCollectionOptions{onlyMetadata: true, currentDescription: func() string { return current }})
			finding := reassessmentFinding()
			seedMetadataReassessment(t, fixture, []domain.Finding{finding}, mode == "metadata edit")
			model.metadataHook = func() { current = "A later description edit changes the review source." }
			job := fixture.job()
			job.MetadataOnly = mode == "metadata edit"
			job.Forced = mode == "forced"
			if err := fixture.run(t.Context(), job); err == nil && mode == "metadata edit" {
				t.Fatal("The review must reject a result for earlier description text.")
			}
			if len(model.metadataPrompts) != 1 {
				t.Fatal("The concurrent edit must occur during explicit reassessment.")
			}
			for _, submitted := range fixture.state.submittedReviews {
				if submitted["state"] == "APPROVED" {
					t.Fatal("The review approved stale description text.")
				}
			}
			stored := retainedMetadataFindings(t, failureSummaryComment(t, fixture))
			if len(stored) != 1 || stored[0].Body != finding.Body {
				t.Fatal("The concurrent edit erased the prior finding.")
			}
		})
	}
}

func TestMetadataReassessmentRequiresModelSupport(t *testing.T) {
	fixture, _ := newContentCollectionFixture(t, &truncatedModel{}, contentCollectionOptions{description: "Current description.", onlyMetadata: true})
	finding := reassessmentFinding()
	seedMetadataReassessment(t, fixture, []domain.Finding{finding}, true)
	job := fixture.job()
	job.MetadataOnly = true
	err := fixture.run(t.Context(), job)
	if err == nil || !strings.Contains(err.Error(), "does not support explicit metadata reassessment") {
		t.Fatalf("The unsupported operation must fail explicitly: %v.", err)
	}
	stored := retainedMetadataFindings(t, failureSummaryComment(t, fixture))
	if len(stored) != 1 || stored[0].Body != finding.Body || len(fixture.state.submittedReviews) != 0 {
		t.Fatal("The unsupported operation must preserve the saved finding without approving.")
	}
}

func TestMetadataReassessmentReanchorsAnOpenFindingAfterHeadingEdits(t *testing.T) {
	passage := "Timestamp repair calls SharedStore.UpdateRowScalars."
	current := "## Problem\n\n" + passage + "\n\n## Changes\n\nThe patch updates timestamp repair."
	decision := metadataDecision("1", domain.ResolutionOpen, passage)
	decision[0].StartLine = 3
	decision[0].EndLine = 3
	decision[0].Reason = "The current description starts with internal method calls instead of explaining the repair problem. Explain the problem before the implementation."
	model := &sequenceModel{results: []domain.ReviewResult{{}}, metadataAssessments: [][]domain.MetadataFindingAssessment{decision}}
	fixture, _ := newContentCollectionFixture(t, model, contentCollectionOptions{description: current, onlyMetadata: true})
	finding := reassessmentFinding()
	finding.Body = "The description starts with a ticket reference instead of explaining the repair problem."
	finding.CorrectionAction = domain.CorrectionReplace
	finding.Suggestion = "The repair updates timestamps without rebuilding the search index."
	seedMetadataReassessment(t, fixture, []domain.Finding{finding}, true)
	job := fixture.job()
	job.MetadataOnly = true
	if err := fixture.run(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	stored := retainedMetadataFindings(t, failureSummaryComment(t, fixture))
	if len(stored) != 1 || stored[0].StartLine != 3 || stored[0].EndLine != 3 || stored[0].CorrectionAction != domain.CorrectionNone || stored[0].Suggestion != "" || stored[0].Body != decision[0].Reason || stored[0].Title != finding.Title || stored[0].RuleID != finding.RuleID {
		t.Fatal("The open finding must use current coordinates without proposing a historical replacement for different lines.")
	}
	if len(model.metadataPrompts) != 1 || !strings.Contains(model.metadataPrompts[0], current) {
		t.Fatal("The reassessment must inspect the complete field after the superficial edit.")
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatal("The superficial edit must continue to request changes.")
	}
	native, ok := fixture.state.lastSubmitReview["body"].(string)
	if !ok || !strings.Contains(native, decision[0].Reason) || strings.Contains(native, finding.Body) {
		t.Fatal("The rejection must explain the current defect without repeating the removed ticket-reference claim.")
	}
}

func TestMetadataReassessmentPreservesFindingsWhenCompleteEvidenceExceedsBudget(t *testing.T) {
	model := &sequenceModel{results: []domain.ReviewResult{{}}}
	fixture, _ := newContentCollectionFixture(t, model, contentCollectionOptions{description: "Current description.", onlyMetadata: true})
	finding := reassessmentFinding()
	finding.Body = strings.Repeat("The saved finding requires complete source assessment. ", 1000)
	seedMetadataReassessment(t, fixture, []domain.Finding{finding}, true)
	job := fixture.job()
	job.MetadataOnly = true
	job.Settings.MaxPromptBytes = 10000
	err := fixture.run(t.Context(), job)
	var sizeFailure *diff.PromptSizeError
	if !errors.As(err, &sizeFailure) {
		t.Fatal("The review must refuse an assessment that cannot include complete evidence.")
	}
	stored := retainedMetadataFindings(t, failureSummaryComment(t, fixture))
	if len(model.prompts) != 1 || len(model.metadataPrompts) != 0 || len(fixture.state.submittedReviews) != 0 || len(stored) != 1 || stored[0].Body != finding.Body {
		t.Fatal("The budget failure must preserve the finding without sending incomplete reassessment evidence or approving.")
	}
}

func TestMetadataRefreshMergesRediscoveredOpenFindings(t *testing.T) {
	for _, resolution := range []domain.Resolution{domain.ResolutionOpen, domain.ResolutionUncertain} {
		t.Run(string(resolution), func(t *testing.T) {
			finding := metadataPresentationFinding()
			current := finding.Evidence
			model := &sequenceModel{
				results:             []domain.ReviewResult{{Findings: []domain.Finding{finding}}, {Findings: []domain.Finding{finding}}},
				metadataAssessments: [][]domain.MetadataFindingAssessment{metadataDecision("1", resolution, finding.Evidence), metadataDecision("1", resolution, finding.Evidence)},
			}
			fixture, _ := newContentCollectionFixture(t, model, contentCollectionOptions{onlyMetadata: true, currentDescription: func() string { return current }})
			seedMetadataReassessment(t, fixture, []domain.Finding{finding}, true)
			job := fixture.job()
			job.MetadataOnly = true
			for iteration := range 2 {
				if iteration > 0 {
					current += "\n\n"
					job.DeliveryID = "metadata-merge-next-revision"
				}
				if err := fixture.run(t.Context(), job); err != nil {
					t.Fatal(err)
				}
				stored := retainedMetadataFindings(t, failureSummaryComment(t, fixture))
				if len(stored) != 1 || stored[0].Body != finding.Body || stored[0].Suggestion != finding.Suggestion || stored[0].CorrectionAction != domain.CorrectionReplace {
					t.Fatal("Fresh discovery must preserve one actionable finding and its current correction after reassessment.")
				}
				if len(model.metadataPrompts) != iteration+1 {
					t.Fatal("Each metadata revision must reassess the saved defect exactly once.")
				}
				native := fixture.state.submittedReviews[len(fixture.state.submittedReviews)-1]
				body, ok := native["body"].(string)
				if !ok || strings.Count(body, finding.Body) != 1 || strings.Count(body, finding.Suggestion) != 1 {
					t.Fatal("The native rejection must render the finding and current correction exactly once.")
				}
			}
		})
	}
}

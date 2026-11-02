package review_test

import (
	"errors"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestMetadataReassessmentFailurePreservesTheStandingRejection(t *testing.T) {
	for _, metadataOnly := range []bool{false, true} {
		for _, failure := range []string{"ungrounded evidence", "missing decision", "provider error"} {
			mode := "ordinary"
			if metadataOnly {
				mode = "metadata edit"
			}
			t.Run(mode+"/"+failure, func(t *testing.T) {
				finding := metadataPresentationFinding()
				current := finding.Evidence
				model := &sequenceModel{results: []domain.ReviewResult{{Findings: []domain.Finding{finding}}, {}}}
				fixture, _ := newContentCollectionFixture(t, model, contentCollectionOptions{currentDescription: func() string { return current }})
				job := fixture.job()
				if err := fixture.run(t.Context(), job); err != nil {
					t.Fatal(err)
				}
				if len(fixture.state.submittedReviews) != 1 {
					t.Fatal("The initial review must publish an actionable metadata rejection.")
				}
				original := fixture.state.submittedReviews[0]
				originalBody := original["body"]
				body := failureSummaryComment(t, fixture)
				state, valid := marker.DecodeState(body)
				if !valid {
					t.Fatal("The original review must preserve durable source coverage.")
				}
				oldState := marker.EncodeState(state)
				state.Status = marker.StateReviewing
				state.LastReviewed = ""
				fixture.state.issueComments[0]["body"] = strings.TrimSuffix(body, oldState) + marker.EncodeState(state)
				current = "## Changes\n\n" + finding.Evidence
				decision := metadataDecision("1", domain.ResolutionResolved, "Text absent from the current description")
				switch failure {
				case "ungrounded evidence":
					model.metadataAssessments = [][]domain.MetadataFindingAssessment{decision}
				case "missing decision":
					model.metadataAssessments = [][]domain.MetadataFindingAssessment{nil}
				case "provider error":
					model.metadataError = errors.New("metadata reassessment provider failed")
				}
				job.DeliveryID = "cosmetic-metadata-reassessment-failure"
				job.MetadataOnly = metadataOnly
				job.AutomaticReassessment = true
				if err := fixture.run(t.Context(), job); err == nil {
					t.Fatal("The invalid reassessment must fail its check.")
				}
				if len(model.metadataPrompts) != 1 || len(fixture.state.dismissals) != 0 || len(fixture.state.submittedReviews) != 1 || original["state"] != "CHANGES_REQUESTED" || original["body"] != originalBody {
					t.Fatal("Failed metadata reassessment must preserve the original rejection without approving or dismissing it.")
				}
				stored := retainedMetadataFindings(t, failureSummaryComment(t, fixture))
				if len(stored) != 1 || stored[0].Title != finding.Title || stored[0].Body != finding.Body || fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
					t.Fatal("The failed check must retain the unresolved metadata finding for a later reassessment.")
				}
			})
		}
	}
}

func TestAnEmptyMetadataCheckpointMustProveResolutionBeforeFailureDismissal(t *testing.T) {
	for _, scenario := range []string{"stale revision", "incomplete assessment"} {
		t.Run(scenario, func(t *testing.T) {
			finding := metadataPresentationFinding()
			model := &sequenceModel{results: []domain.ReviewResult{{Findings: []domain.Finding{finding}}}}
			fixture, _ := newContentCollectionFixture(t, model, contentCollectionOptions{onlyMetadata: true, description: finding.Evidence})
			job := fixture.job()
			if err := fixture.run(t.Context(), job); err != nil {
				t.Fatal(err)
			}
			original := fixture.state.submittedReviews[0]
			originalBody := original["body"]
			seedMetadataReassessment(t, fixture, nil, scenario == "stale revision")
			if scenario == "incomplete assessment" {
				current, err := fixture.github.GetPullRequest(t.Context(), job.InstallationID, job.Repository, job.Number)
				if err != nil {
					t.Fatal(err)
				}
				summary := testSummary()
				summary.MetadataRevision = diff.PullRequestRevision(current)
				state := decodedSummaryState(t, fixture)
				fixture.state.issueComments[0]["body"] = review.RenderBody(summary) + "\n" + marker.EncodeState(state)
				fixture.reconciler.err = errors.New("thread reconciliation failed before metadata reassessment")
				job.AutomaticReassessment = true
			} else {
				model.err = errors.New("provider failed before metadata reassessment")
			}
			job.MetadataOnly = true
			job.DeliveryID = "empty-checkpoint-failed-reassessment"
			if err := fixture.run(t.Context(), job); err == nil {
				t.Fatal("The operational failure must fail its check.")
			}
			if len(fixture.state.dismissals) != 0 || len(fixture.state.submittedReviews) != 1 || original["state"] != "CHANGES_REQUESTED" || original["body"] != originalBody {
				t.Fatal("An empty checkpoint without completed current assessment must not withdraw an existing metadata rejection.")
			}
		})
	}
}

package review_test

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestMetadataRefreshRejectsAnUnterminatedSummaryCheckpoint(t *testing.T) {
	current := "The repair updates existing timestamps without rebuilding the search index."
	model := &sequenceModel{
		results: []domain.ReviewResult{{}},
		metadataAssessments: [][]domain.MetadataFindingAssessment{
			metadataDecision("1", domain.ResolutionResolved, current),
		},
	}
	fixture, _ := newContentCollectionFixture(
		t,
		model,
		contentCollectionOptions{
			description:  current,
			onlyMetadata: true,
		},
	)
	job := fixture.job()
	job.MetadataOnly = true
	prior := findingDetailsSummary()
	prior.Metadata = []domain.Finding{reassessmentFinding()}
	prior.MetadataRevision = "previous-metadata-revision"
	fixture.state.reviewPages = [][]map[string]any{
		{
			{
				"id":                         float64(4100),
				collectionFixtureCommitIDKey: string(fixture.job().Head),
				collectionFixtureStateKey:    collectionFixtureRejectionState,

				"body": review.RenderVerdictBody(prior),
				"user": map[string]any{"login": testBotLogin},
			},
		},
	}
	state := marker.State{
		LastReviewed: job.Head,
		RunID:        "previous-review",
		Status:       marker.StateDone,
	}
	fixture.state.issueComments = []map[string]any{
		{
			"id":   float64(2000),
			"user": map[string]any{"login": testBotLogin},
			"body": marker.Summary() + "\n<!-- pr-review-agent:metadata:v1 unfinished\n" + marker.EncodeState(
				state,
			),
		},
	}

	err := fixture.run(t.Context(), job)
	if err == nil ||
		!strings.Contains(err.Error(), "cannot replace an incomplete metadata review state") {
		t.Fatalf(
			"The malformed summary checkpoint must fail explicitly before publishing the resolved assessment: %v.",
			err,
		)
	}

	if len(model.prompts) != 1 || len(model.metadataPrompts) != 1 {
		t.Fatal(
			"The regression must complete fresh metadata review and explicitly resolve the finding recovered from the native review.",
		)
	}

	if len(fixture.state.submittedReviews) != 0 || fixture.state.lastSubmitReview != nil ||
		fixture.state.lastUpdateReview != nil {
		t.Fatal("The failed checkpoint replacement must not submit or update a native review.")
	}
}

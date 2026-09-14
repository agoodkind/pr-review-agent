package review_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

func TestARejectedOmissionWithoutFindingsPublishesOnlyAComment(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         oversizedHunkCollector{},
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{{
			OmissionsAcceptable: false,
			DecisionReason:      "The unread change cannot be checked from the available context.",
		}}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionComment) {
		t.Fatalf("event = %v, want COMMENT without an actionable finding", fixture.state.lastSubmitReview["event"])
	}
}

func TestARejectedOmissionRefreshRemovesABlockWithoutAnInlineFinding(t *testing.T) {
	fixture := rejectedOmissionFixture(t)
	seedStateNaming(fixture, testHeadSHA)

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionComment) {
		t.Fatalf("event = %v, want COMMENT without an actionable finding", fixture.state.lastSubmitReview["event"])
	}
	if len(fixture.state.dismissals) != 1 {
		t.Fatalf("dismissals = %v, want the unsupported earlier block removed", fixture.state.dismissals)
	}
}

func TestARejectedOmissionRefreshPreservesABlockWithAnOpenInlineFinding(t *testing.T) {
	fixture := rejectedOmissionFixture(t)
	seedStateNaming(fixture, testHeadSHA)
	openThread := resolvedBotThread("thread-open")
	openThread.Resolved = false
	fixture.state.threadNodes = threadNodesFor([]githubapp.ReviewThread{openThread})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastSubmitReview != nil {
		t.Fatalf("submitted review = %v, want the standing block retained", fixture.state.lastSubmitReview)
	}
	if len(fixture.state.dismissals) != 0 {
		t.Fatalf("dismissals = %v, want the block on the open finding retained", fixture.state.dismissals)
	}
	if body := failureSummaryComment(t, fixture); !strings.Contains(body, "Resolve the open inline findings.") {
		t.Fatalf("summary = %q, want the retained requested-changes verdict", body)
	}
}

func rejectedOmissionFixture(t *testing.T) *serviceFixture {
	t.Helper()
	metadata := `[{"path":"huge.go","header":"@@ -1,1 +1,2101 @@","reason":"hunk exceeds one model request"}]`
	body := marker.Review(domain.HeadSHA(testHeadSHA), domain.ReviewDecisionRequestChanges) +
		"\n<!-- pr-review-agent:omissions:v1 " + base64.RawURLEncoding.EncodeToString([]byte(metadata)) + " -->" +
		"\n<!-- pr-review-agent:omissions-acceptable:v1 false -->"
	return newServiceFixture(t, serviceFixtureOptions{
		reviewPages: [][]map[string]any{{{
			"id":        float64(4100),
			"commit_id": testHeadSHA,
			"state":     "CHANGES_REQUESTED",
			"body":      body,
			"user":      map[string]any{"login": testBotLogin},
		}}},
	})
}

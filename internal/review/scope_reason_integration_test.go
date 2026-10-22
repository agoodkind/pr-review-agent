package review_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestScopeReasonsRequireActualOmissionsOrScopeAssessments(t *testing.T) {
	policy := policytest.Load(t)
	file := classificationGitFile(t, "prose.md", "Previous source.\n", "The supplied prose explains the current change.\n")
	pullRequest := githubapp.PullRequest{Head: domain.HeadSHA(testHeadSHA), Title: "Update prose", Body: "Review the complete supplied prose."}
	for _, scenario := range []struct {
		name        string
		action      domain.FileReviewAction
		requirement domain.ContentReviewRequirement
		kind        reviewrules.ContentKind
		reviewed    bool
		accepted    bool
		wantReason  bool
	}{
		{name: "complete supplied source"},
		{name: "fully inspected assessment", action: domain.FileActionReview, requirement: domain.ContentReviewRequired, kind: reviewrules.ContentSource, reviewed: true},
		{name: "necessary encrypted gap", action: domain.FileActionGap, requirement: domain.ContentReviewRequired, kind: reviewrules.ContentEncrypted, wantReason: true},
		{name: "accepted encrypted exclusion", action: domain.FileActionExclude, requirement: domain.ContentReviewUnnecessary, kind: reviewrules.ContentEncrypted, accepted: true, wantReason: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var assessments []domain.FileAssessment
			if scenario.action != "" {
				assessment := classificationAssessment(t, file, policy, scenario.requirement, scenario.action, scenario.kind)
				planned, decisions, err := review.PlanFileContentReview(diff.ReviewInput{PullRequest: pullRequest, Files: []diff.FileContext{file}}, []domain.FileAssessment{assessment}, policy)
				if err != nil {
					t.Fatal(err)
				}
				if scenario.reviewed {
					assertPlannedChangedRows(t, planned, file)
					decisions[0].Reviewed = true
				}
				assessments = decisions
			}
			const reason = "The model reported a reason about the available review scope."
			var summary review.Summary
			summary.Head = pullRequest.Head
			summary.Decision = domain.ReviewDecisionApprove
			summary.DecisionReason = reason
			summary.CoverageComplete = scenario.action == "" || scenario.reviewed
			summary.OmissionsAccepted = scenario.accepted
			summary.FileAssessments = assessments
			summary.FileAssessmentMaximumBytes = policy.FileAssessmentMaximumBytes()
			summary.MetadataRevision = diff.PullRequestRevision(pullRequest)
			body := review.RenderBody(summary)
			if strings.Contains(body, "#### Review scope\n\n"+reason) != scenario.wantReason || strings.Contains(body, "<!-- pr-review-agent:decision-reason:v1 ") != scenario.wantReason {
				t.Fatal("The visible and saved scope reason must agree with the actual omissions or contextual assessment.")
			}
			if !review.MetadataMatches(body, summary.Head, summary.MetadataRevision) {
				t.Fatal("Scope reason filtering must preserve the current metadata review record.")
			}
			if scopeReasonMetadataRecord(t, body) != "" {
				t.Fatal("A file scope reason must not become a claim about missing pull request prose.")
			}
		})
	}
}

func scopeReasonMetadataRecord(t *testing.T, body string) string {
	t.Helper()
	_, encoded, found := strings.Cut(body, "<!-- pr-review-agent:metadata:v1 ")
	if !found {
		t.Fatal("The published summary omitted its metadata record.")
	}
	encoded, _, _ = strings.Cut(encoded, " -->")
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		DecisionReason string `json:"decision_reason"`
	}
	if err := json.Unmarshal(payload, &record); err != nil {
		t.Fatal(err)
	}
	return record.DecisionReason
}

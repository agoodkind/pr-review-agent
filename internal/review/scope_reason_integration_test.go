package review_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestInspectedContentPreservesUnresolvedFileScope(t *testing.T) {
	policy := policytest.Load(t)
	file := classificationGitFile(t, "credentials.vault", "$ANSIBLE_VAULT;1.1;AES256\n61616161\n", "$ANSIBLE_VAULT;1.1;AES256\n62626262\n")
	pr := githubapp.PullRequest{Head: domain.HeadSHA(testHeadSHA), Title: "Update fixture content"}
	for _, scenario := range []struct {
		name        string
		action      domain.FileReviewAction
		requirement domain.ContentReviewRequirement
		scope       string
	}{
		{name: "ordinary technical findings", scope: "yes"},
		{name: "ordinary content review", action: domain.FileActionReview, requirement: domain.ContentReviewRequired, scope: "yes"},
		{name: "necessary semantic gap", action: domain.FileActionGap, requirement: domain.ContentReviewRequired, scope: "no"},
		{name: "necessary related evidence", action: domain.FileActionInspectRelated, requirement: domain.ContentReviewRequired, scope: "no"},
		{name: "accepted exclusion", action: domain.FileActionExclude, requirement: domain.ContentReviewUnnecessary, scope: "yes"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var assessments []domain.FileAssessment
			input := diff.ReviewInput{PullRequest: pr, Files: []diff.FileContext{file}}
			planned := input
			if scenario.action != "" {
				assessment := classificationAssessment(t, file, policy, scenario.requirement, scenario.action, reviewrules.ContentEncrypted)
				var err error
				planned, assessments, err = review.PlanFileContentReview(input, []domain.FileAssessment{assessment}, policy)
				if err != nil {
					t.Fatal(err)
				}
			}
			excluded := scenario.action == domain.FileActionExclude
			if !excluded {
				assertPlannedChangedRows(t, planned, file)
			}
			var summary review.Summary
			summary.Head = pr.Head
			summary.Decision = domain.ReviewDecisionRequestChanges
			summary.CoverageComplete = !excluded
			summary.OmissionsAccepted = excluded
			summary.FileAssessments = assessments
			summary.FileAssessmentMaximumBytes = policy.FileAssessmentMaximumBytes()
			summary.MetadataRevision = diff.PullRequestRevision(pr)
			body := review.RenderBody(summary)
			if !strings.Contains(body, "| Review scope assessed | "+scenario.scope+" |") {
				t.Fatal("The scope row must distinguish inspected content from unresolved semantic requirements.")
			}
			content := "yes"
			if excluded {
				content = "no"
			}
			if !strings.Contains(body, "| All file content inspected | "+content+" |") {
				t.Fatal("The content inspection row must preserve the actual planned source coverage.")
			}
			head, found := marker.FindReview(body)
			if !found || head != pr.Head || !review.MetadataMatches(body, pr.Head, summary.MetadataRevision) {
				t.Fatal("The rendered assessment must preserve its current review and metadata records.")
			}
		})
	}
}

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

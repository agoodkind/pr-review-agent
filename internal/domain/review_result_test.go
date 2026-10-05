package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestRuleImportanceDeterminesPublishedReviewDecision(t *testing.T) {
	catalog := policytest.Load(t).Catalog()
	cases := []struct {
		name               string
		rule               reviewrules.ID
		reported           int
		overrides          reviewrules.Importance
		minimum            int
		expectedImportance int
		expectedDecision   domain.ReviewDecision
	}{
		{name: "Prose receives maximum importance", rule: "complete_sentences", reported: 1, overrides: nil, minimum: 10, expectedImportance: 10, expectedDecision: domain.ReviewDecisionRequestChanges},
		{name: "Configured importance lowers publication eligibility", rule: "shared_boundaries", reported: 9, overrides: reviewrules.Importance{"shared_boundaries": 4}, minimum: 7, expectedImportance: 4, expectedDecision: domain.ReviewDecisionApprove},
		{name: "Configured importance raises publication eligibility", rule: "shared_boundaries", reported: 2, overrides: reviewrules.Importance{"shared_boundaries": 8}, minimum: 7, expectedImportance: 8, expectedDecision: domain.ReviewDecisionRequestChanges},
		{name: "Unconfigured technical rules retain reported importance", rule: "shared_boundaries", reported: 4, overrides: nil, minimum: 7, expectedImportance: 4, expectedDecision: domain.ReviewDecisionApprove},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			encoded, err := json.Marshal(domain.ReviewResult{Overview: "The change updates a source comment.", OmissionsAcceptable: true, DecisionReason: "The supplied source permits review.", Findings: []domain.Finding{{
				RuleID: item.rule, Path: "example.go", StartLine: 1, EndLine: 1,
				Title: "Clarify the source comment", Body: "The source comment requires a complete sentence.",
				Evidence: "source comment", Claim: "The source comment is incomplete.", Suggestion: "", Importance: item.reported,
			}}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := domain.UnmarshalReviewResult(encoded, catalog, item.overrides)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Findings) != 1 || result.Findings[0].Importance != item.expectedImportance {
				t.Fatalf("The decoded review has incorrect importance: %+v", result.Findings)
			}
			decision := review.DecisionFor(result.Findings, item.minimum)
			if decision != item.expectedDecision {
				t.Fatalf("The published decision is %s; expected %s.", decision, item.expectedDecision)
			}
		})
	}
}

func TestMetadataFeedbackWithoutAnExactCorrectionRequestsChanges(t *testing.T) {
	policy := policytest.Load(t)
	for _, target := range []domain.FindingTarget{
		{Surface: domain.FindingPullRequestTitle},
		{Surface: domain.FindingPullRequestDescription},
		{Surface: domain.FindingCommitMessage, CommitSHA: "cbb5fe583ff3098e64d36b3174cad83ad9ac0348"},
	} {
		t.Run(string(target.Surface), func(t *testing.T) {
			finding := domain.Finding{
				Surface: target.Surface, CommitSHA: target.CommitSHA,
				CorrectionAction: domain.CorrectionNone, RuleID: "process_narration",
				StartLine: 1, EndLine: 1, Title: "Remove process narration",
				Body:     "The selected source line narrates work instead of explaining the change.",
				Evidence: "I am checking CI, then merging and deploying.",
				Claim:    "The source line contains process narration.", Importance: 1,
			}
			encoded, err := json.Marshal(domain.ReviewResult{
				Overview:            "The review identified process narration.",
				OmissionsAcceptable: true, Findings: []domain.Finding{finding},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := domain.UnmarshalReviewResult(encoded, policy.Catalog(), nil)
			if err != nil {
				t.Fatal(err)
			}
			decision := review.DecisionFor(result.Findings, 10)
			if decision != domain.ReviewDecisionRequestChanges {
				t.Fatalf("metadata feedback produced %s instead of requested changes", decision)
			}
			body := review.RenderBody(review.Summary{
				Decision: decision, Metadata: result.Findings,
			})
			if !strings.Contains(body, finding.Body) || !strings.Contains(body, "Correct the prose findings below.") {
				t.Fatalf("the published summary omitted metadata feedback: %s", body)
			}
			if strings.Contains(body, "Replace those lines with:") || strings.Contains(body, "Delete lines") {
				t.Fatalf("the published summary invented an unavailable correction: %s", body)
			}
		})
	}
}

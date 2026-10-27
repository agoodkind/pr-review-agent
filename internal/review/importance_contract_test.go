package review_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestJobThresholdDoesNotConflictWithSystemClassification(t *testing.T) {
	policy := policytest.Load(t)
	startup, err := review.PolicyHeader(policy, 6, nil)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := review.PolicyHeader(policy, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if startup != delivery || !strings.Contains(startup, "Classify every concrete defect from importance 1 through 10.") {
		t.Fatal("The system instructions must classify all scores without imposing a startup publication threshold.")
	}
	var chunk diff.Chunk
	chunk.Index, chunk.Total, chunk.Text = 1, 1, "Changed source."
	const maximumPromptBytes = 100_000
	prompt, err := review.BuildPrompt(chunk, 5, "", policy, maximumPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "importance 5 or higher") || !strings.Contains(prompt, "Do not omit a real defect because it is below that publication threshold.") {
		t.Fatal("The job prompt must use its publication threshold while requesting findings below it.")
	}
}

func TestPublishedVerdictUsesTheFindingThreshold(t *testing.T) {
	policy := policytest.Load(t)
	for _, scenario := range []struct {
		name      string
		rule      reviewrules.ID
		reported  int
		minimum   int
		effective int
		decision  domain.ReviewDecision
	}{
		{name: "below five", rule: "shared_boundaries", reported: 4, minimum: 5, effective: 4, decision: domain.ReviewDecisionApprove},
		{name: "at five", rule: "shared_boundaries", reported: 5, minimum: 5, effective: 5, decision: domain.ReviewDecisionRequestChanges},
		{name: "below six", rule: "shared_boundaries", reported: 5, minimum: 6, effective: 5, decision: domain.ReviewDecisionApprove},
		{name: "prose retains ten", rule: "complete_sentences", reported: 1, minimum: 10, effective: 10, decision: domain.ReviewDecisionRequestChanges},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var finding domain.Finding
			finding.RuleID, finding.Path, finding.StartLine, finding.EndLine = scenario.rule, "example.go", 1, 1
			finding.Title, finding.Body = "Use a shared boundary", "The operations require one shared boundary."
			finding.Evidence, finding.Claim, finding.Importance = "source", "The operations use separate boundaries.", scenario.reported
			var result domain.ReviewResult
			result.Overview, result.OmissionsAcceptable, result.Findings = "The change updates an operation.", true, []domain.Finding{finding}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := domain.UnmarshalReviewResult(encoded, policy.Catalog(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(decoded.Findings) != 1 || decoded.Findings[0].Importance != scenario.effective {
				t.Fatalf("The catalog produced incorrect finding importance: %+v", decoded.Findings)
			}
			var summary review.Summary
			summary.MinimumImportance = scenario.minimum
			summary.Decision = review.DecisionFor(decoded.Findings, scenario.minimum)
			if summary.Decision != scenario.decision {
				t.Fatalf("The decision is %s; expected %s.", summary.Decision, scenario.decision)
			}
			body := review.RenderBody(summary)
			if summary.Decision == domain.ReviewDecisionApprove {
				threshold := fmt.Sprintf("| Minimum importance | `%d` |", scenario.minimum)
				if !strings.Contains(body, "This review did not identify findings at or above the minimum importance.") || !strings.Contains(body, threshold) {
					t.Fatalf("The approval does not state its finding threshold: %s", body)
				}
			} else if !strings.Contains(body, "Resolve the open inline findings.") {
				t.Fatalf("The verdict does not request changes for an eligible finding: %s", body)
			}
		})
	}
}

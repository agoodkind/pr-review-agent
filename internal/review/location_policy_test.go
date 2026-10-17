package review_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestConfiguredLocationsRetainSeparateInlineIdentities(t *testing.T) {
	policy, ruleID := loadLocationPolicy(t)
	first := domain.Finding{
		RuleID: ruleID, Path: "guide.md", StartLine: 2, EndLine: 2,
		Title: "Complete the contrast", Body: "The paragraph needs a complete explanation.",
		Evidence: "The same source sentence.", Claim: "The explanation omits its subject.", Importance: 1,
	}
	second := first
	second.StartLine, second.EndLine = 8, 8
	encoded, err := json.Marshal(domain.ReviewResult{Findings: []domain.Finding{first, second}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := domain.UnmarshalReviewResult(encoded, policy.Catalog(), nil)
	if err != nil {
		t.Fatal(err)
	}
	comments, err := review.RenderInline(domain.HeadSHA(testHeadSHA), result.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 {
		t.Fatalf("inline comments = %d, expected both locations", len(comments))
	}
	identities := make(map[string]struct{})
	for index, finding := range result.Findings {
		if finding.Importance != 10 || finding.Deduplication != reviewrules.DeduplicateLocation {
			t.Fatalf("configured finding policy = %+v", finding)
		}
		published, ok := marker.FindFinding(comments[index].Body)
		if !ok {
			t.Fatal("the inline comment omitted its durable finding identity")
		}
		identities[published.ID] = struct{}{}
		_, restored, err := marker.DecodeFindingBody(domain.ReviewComment{
			Body: comments[index].Body, Path: finding.Path,
			StartLine: finding.StartLine, EndLine: finding.EndLine,
		})
		if err != nil || restored.Deduplication != reviewrules.DeduplicateLocation {
			t.Fatalf("durable location policy = %+v, error = %v", restored, err)
		}
	}
	if len(identities) != 2 {
		t.Fatal("separately editable source locations share an inline identity")
	}
	if review.DecisionFor(result.Findings[1:], 10) != domain.ReviewDecisionRequestChanges {
		t.Fatal("the remaining occurrence did not prevent approval")
	}
	legacyBody, err := marker.EncodeFindingBody(domain.HeadSHA(testHeadSHA), first)
	if err != nil {
		t.Fatal(err)
	}
	_, legacy, err := marker.DecodeFindingBody(domain.ReviewComment{
		Body: legacyBody, Path: first.Path, StartLine: first.StartLine, EndLine: first.EndLine,
	})
	if err != nil || legacy.Deduplication != "" {
		t.Fatalf("legacy finding = %+v, error = %v", legacy, err)
	}
}

func loadLocationPolicy(t *testing.T) (reviewrules.Policy, reviewrules.ID) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "review-rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog map[string]json.RawMessage
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	var rules []map[string]json.RawMessage
	if err := json.Unmarshal(catalog["rules"], &rules); err != nil {
		t.Fatal(err)
	}
	rules[0]["deduplication"] = json.RawMessage(`"location"`)
	rules[0]["category"] = json.RawMessage(`"prose"`)
	var ruleID reviewrules.ID
	if err := json.Unmarshal(rules[0]["id"], &ruleID); err != nil {
		t.Fatal(err)
	}
	catalog["rules"], err = json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := reviewrules.LoadPolicy(path, filepath.Join("..", "..", "config", "review-prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return policy, ruleID
}

func TestLocationMarkersRetainIdentityAfterGitHubRemapsTheSource(t *testing.T) {
	finding := domain.Finding{
		Path: "guide.md", StartLine: 2, EndLine: 3,
		Title: "Complete the contrast", Body: "The paragraph needs a complete explanation.",
		Importance: 10, Deduplication: reviewrules.DeduplicateLocation,
	}
	body, err := marker.EncodeFindingBody(domain.HeadSHA(testHeadSHA), finding)
	if err != nil {
		t.Fatal(err)
	}
	_, restored, err := marker.DecodeFindingBody(domain.ReviewComment{
		Body: body, Path: finding.Path, StartLine: 9, EndLine: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.StartLine != 9 || restored.EndLine != 10 || restored.Deduplication != reviewrules.DeduplicateLocation {
		t.Fatalf("the decoded finding did not retain its current source range: %+v", restored)
	}
	published, ok := marker.FindFinding(body)
	if !ok || published.OriginalStartLine != 2 || published.OriginalEndLine != 3 {
		t.Fatalf("the marker did not preserve its publication range: %+v", published)
	}
	finding.Deduplication = ""
	legacyBody, err := marker.EncodeFindingBody(domain.HeadSHA(testHeadSHA), finding)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := marker.DecodeFindingBody(domain.ReviewComment{
		Body: legacyBody, Path: finding.Path, StartLine: 9, EndLine: 10,
	}); err != nil {
		t.Fatalf("the remapped legacy marker failed to decode: %v", err)
	}
}

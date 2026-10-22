package review_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestConsolidationPromptPreservesFindingsWithinActualProviderBudget(t *testing.T) {
	policy := policytest.Load(t)
	client := stageBudgetClient(policy)
	candidates := []domain.Finding{
		{RuleID: "technical_relationships", Surface: domain.FindingFile, Path: "writer.go", StartLine: 3, EndLine: 3, Title: "Keep the transaction open", Claim: "The writer commits before deleting the old record.", Body: "The writer commits before deleting the old record. Preserve both operations in one transaction.", Evidence: "transaction.Commit()", Importance: 9},
		{RuleID: "complete_sentences", Surface: domain.FindingFile, Path: "writer.go", StartLine: 7, EndLine: 7, Title: "Complete the explanation", Claim: "The explanation omits its predicate.", Body: "The explanation omits its predicate. State the operation in a full sentence.", Evidence: "// The record that contains café values.", Importance: 10},
	}
	for _, carried := range [][]domain.Finding{nil, {candidates[0]}} {
		required, err := review.BuildConsolidationPrompt(client, candidates, carried, "", config.MaximumPromptBytes, policy)
		if err != nil {
			t.Fatal(err)
		}
		minimum, err := client.ConsolidateRequestBytes(required)
		if err != nil {
			t.Fatal(err)
		}
		maximum := minimum + 2048
		markers := strings.Split(policy.WrapUntrusted(""), "\n")
		optional := strings.Repeat("Related source discusses café values. "+markers[len(markers)-1]+"\n", maximum)
		prompt, err := review.BuildConsolidationPrompt(client, candidates, carried, optional, maximum, policy)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := client.ConsolidateRequestBytes(prompt)
		if err != nil || actual > maximum {
			t.Fatalf("The consolidation request exceeds its serialized provider budget: %d/%d, %v.", actual, maximum, err)
		}
		for _, finding := range append(append([]domain.Finding{}, candidates...), carried...) {
			for _, required := range []string{finding.Path, finding.Title, finding.Claim, finding.Body, finding.Evidence, string(finding.RuleID)} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("Consolidation truncated required finding data %q.", required)
				}
			}
		}
		assertStageBoundaries(t, policy, prompt, "consolidate.context_omitted")
		oversized := append([]domain.Finding{}, candidates...)
		oversized[0].Evidence = strings.Repeat("necessary source evidence", maximum)
		prompt, err = review.BuildConsolidationPrompt(client, oversized, carried, "", maximum, policy)
		var refused *diff.PromptSizeError
		if prompt != "" || !errors.As(err, &refused) || refused.Actual <= maximum || refused.Limit != maximum {
			t.Fatal("Oversized required candidate evidence must produce a typed local refusal without truncation.")
		}
	}
}

func TestReportPromptFitsActualProviderBudgetWithoutBreakingSourceBoundaries(t *testing.T) {
	policy := policytest.Load(t)
	client := stageBudgetClient(policy)
	pr := githubapp.PullRequest{Title: "Preserve writer transactions", Body: "The writer deletes stale records within its transaction."}
	minimumPrompt, err := review.BuildReportPrompt(client, pr, []string{"The writer deletes stale records within its transaction."}, config.MaximumPromptBytes, policy)
	if err != nil {
		t.Fatal(err)
	}
	minimum, err := client.ReportRequestBytes(minimumPrompt)
	if err != nil {
		t.Fatal(err)
	}
	maximum := minimum + 2048
	markers := strings.Split(policy.WrapUntrusted(""), "\n")
	pr.Body = strings.Repeat("The change preserves café records. "+markers[len(markers)-1]+"\n", maximum)
	prompt, err := review.BuildReportPrompt(client, pr, []string{strings.Repeat("The writer retains transaction ownership. ", maximum)}, maximum, policy)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := client.ReportRequestBytes(prompt)
	if err != nil || actual > maximum {
		t.Fatalf("The report request exceeds its serialized provider budget: %d/%d, %v.", actual, maximum, err)
	}
	assertStageBoundaries(t, policy, prompt, "report.context_omitted")
	_, err = review.BuildReportPrompt(client, pr, nil, 1, policy)
	var refused *diff.PromptSizeError
	if !errors.As(err, &refused) || refused.Actual <= refused.Limit {
		t.Fatal("A report budget below its required instructions must produce a typed local refusal.")
	}
}

func stageBudgetClient(policy reviewrules.Policy) *openai.Client {
	return openai.NewClient(config.Config{ReviewPolicy: policy, MinimumImportance: 7, Providers: []config.ProviderConfig{
		{ID: "responses", Model: "configured-response-model", API: config.ResponsesAPI},
		{ID: "chat", Model: "configured-chat-model", API: config.ChatCompletionsAPI},
		{ID: "gemini", Model: "configured-gemini-model", API: config.GeminiAPI},
	}}, nil)
}

func assertStageBoundaries(t *testing.T, policy reviewrules.Policy, prompt, noteTemplate string) {
	t.Helper()
	markers := strings.Split(policy.WrapUntrusted(""), "\n")
	if !utf8.ValidString(prompt) || strings.Count(prompt, markers[0]) == 0 || strings.Count(prompt, markers[0]) != strings.Count(prompt, markers[len(markers)-1]) {
		t.Fatal("The bounded request must retain complete untrusted input boundaries and valid UTF-8.")
	}
	var data reviewrules.PromptData
	note, err := policy.Render(noteTemplate, data)
	if err != nil || !strings.Contains(prompt, note) {
		t.Fatal("The bounded request must disclose omitted optional context using its configured note.")
	}
}

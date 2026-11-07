package review_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestDisallowedExclusionPreservesRequiredSourceThroughReview(t *testing.T) {
	const before = "net.ipv4.ip_forward=1\nnet.ipv6.conf.all.forwarding=1\nnet.ipv6.conf.vmbr1.disable_ipv6=0\n"
	const after = "net.ipv4.ip_forward=1\nnet.ipv6.conf.all.forwarding=1\nnet.ipv6.conf.uplink.disable_ipv6=0\n"
	policy := policytest.Load(t)
	file := classificationGitFile(t, "network/forwarding.conf", before, after)
	input := diff.ReviewInput{Files: []diff.FileContext{file}}
	assessment := classificationAssessment(t, file, policy, domain.ContentReviewUnnecessary, domain.FileActionExclude, reviewrules.ContentSource)
	assessment.Reason = "The complete file content is visible in the supplied excerpt, so further inspection is unnecessary."
	planned, decisions, err := review.PlanFileContentReview(input, []domain.FileAssessment{assessment}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != domain.FileActionReview || decisions[0].ContentReviewRequirement != domain.ContentReviewRequired || decisions[0].Reviewed {
		t.Fatal("A policy-forbidden exclusion must require ordinary source review before it can complete.")
	}
	assertPlannedChangedRows(t, planned, file)
	chunks, err := diff.ChunkInput(planned, config.MaximumPromptBytes)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("The readable configuration must produce one complete review chunk: %v", err)
	}
	client := stageBudgetClient(policy)
	prompt, err := review.BuildModelReviewPrompt(client, chunks[0], 5, "", "", policy, config.MaximumPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	payloads, err := client.ReviewRequestPayloads(prompt)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range payloads {
		if !strings.Contains(string(payload.Body), "net.ipv6.conf.uplink.disable_ipv6=0") {
			t.Fatal("Every configured provider must receive the required changed configuration.")
		}
	}
	decisions[0].Reviewed = true
	body := review.RenderBody(review.Summary{Decision: domain.ReviewDecisionApprove, CoverageComplete: true, FileAssessments: decisions, FileAssessmentMaximumBytes: policy.FileAssessmentMaximumBytes(), FileAssessmentsDisplay: policy.FileAssessmentsDisplay()})
	if !strings.Contains(body, "| Review scope assessed | yes |") || strings.Contains(body, "| source | exclude |") {
		t.Fatal("Completed source review must not retain the invalid exclusion as an unresolved scope decision.")
	}
}

func TestDisallowedExclusionCannotHideSuppliedAssessmentEvidence(t *testing.T) {
	policy := policytest.Load(t)
	file := classificationGitFile(t, "network/forwarding.conf", "net.ipv6.conf.vmbr1.disable_ipv6=0\n", "net.ipv6.conf.uplink.disable_ipv6=0\n")
	assessment := classificationAssessment(t, file, policy, domain.ContentReviewUnnecessary, domain.FileActionExclude, reviewrules.ContentSource)
	client := stageBudgetClient(policy)
	prompt, err := review.BuildFileAssessmentPrompt(client, githubapp.PullRequest{}, []diff.FileContext{file}, file.Path, policy, config.MaximumPromptBytes, []domain.FileAssessment{assessment})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, strings.TrimSuffix(file.CurrentContent, "\n")) {
		t.Fatal("A policy-forbidden exclusion must not remove readable evidence from scope reassessment.")
	}
}

func TestConfiguredExclusionPermissionsControlPlanningAndEvidence(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		name := "source_review_required"
		if allowed {
			name = "source_exclusion_allowed"
		}
		t.Run(name, func(t *testing.T) {
			policy := exclusionPolicy(t, allowed)
			file := classificationGitFile(t, "configuration.custom", "earlier value\n", "current readable value\n")
			assessment := classificationAssessment(t, file, policy, domain.ContentReviewUnnecessary, domain.FileActionExclude, reviewrules.ContentSource)
			planned, decisions, err := review.PlanFileContentReview(diff.ReviewInput{Files: []diff.FileContext{file}}, []domain.FileAssessment{assessment}, policy)
			if err != nil {
				t.Fatal(err)
			}
			if allowed {
				if len(planned.Files) != 0 || !decisions[0].PermitsContentExclusion() {
					t.Fatal("An explicitly permitted category must retain its justified exclusion.")
				}
				return
			}
			if decisions[0].Action != domain.FileActionReview || decisions[0].ContentReviewRequirement != domain.ContentReviewRequired {
				t.Fatal("The configured category permission must control the saved scope action.")
			}
			assertPlannedChangedRows(t, planned, file)
		})
	}
}

func exclusionPolicy(t *testing.T, allowSource bool) reviewrules.Policy {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "config", "review-prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prompts map[string]json.RawMessage
	if err := json.Unmarshal(content, &prompts); err != nil {
		t.Fatal(err)
	}
	settings := policytest.Load(t).FileClassification()
	if allowSource {
		settings.AllowedExclusions = append(settings.AllowedExclusions, reviewrules.ContentSource)
	}
	prompts["file_classification"], err = json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	content, err = json.Marshal(prompts)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "review-prompts.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := reviewrules.LoadPolicy(filepath.Join("..", "..", "config", "review-rules.json"), path)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

package review_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestFileReviewRequirementPreservesNecessaryGitSourceAndSemanticGaps(t *testing.T) {
	var source strings.Builder
	source.WriteString("-- Generated migration.\n")
	for index := range 1000 {
		fmt.Fprintf(&source, "INSERT INTO audit VALUES (%d, '%s');\n", index, strings.Repeat("fixture", 50))
	}
	source.WriteString("DROP TABLE users;\n")
	file := classificationGitFile(t, "migration.sql", "-- Previous migration.\n", source.String())
	policy := policytest.Load(t)
	for _, scenario := range []struct {
		name        string
		requirement domain.ContentReviewRequirement
		action      domain.FileReviewAction
		kind        reviewrules.ContentKind
		wantAction  domain.FileReviewAction
		excluded    bool
	}{
		{name: "necessary_behavior", requirement: domain.ContentReviewRequired, action: domain.FileActionReview, kind: reviewrules.ContentGenerated, wantAction: domain.FileActionReview},
		{name: "contradictory_exclusion", requirement: domain.ContentReviewRequired, action: domain.FileActionExclude, kind: reviewrules.ContentGenerated, wantAction: domain.FileActionGap},
		{name: "uncertain_exclusion", requirement: domain.ContentReviewUncertain, action: domain.FileActionExclude, kind: reviewrules.ContentGenerated, wantAction: domain.FileActionGap},
		{name: "justified_exclusion", requirement: domain.ContentReviewUnnecessary, action: domain.FileActionExclude, kind: reviewrules.ContentGenerated, wantAction: domain.FileActionExclude, excluded: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assessment := classificationAssessment(t, file, policy, scenario.requirement, scenario.action, scenario.kind)
			planned, decisions, err := review.PlanFileContentReview(diff.ReviewInput{Files: []diff.FileContext{file}}, []domain.FileAssessment{assessment}, policy)
			if err != nil {
				t.Fatal(err)
			}
			if len(decisions) != 1 || decisions[0].Action != scenario.wantAction || decisions[0].Reviewed {
				t.Fatal("The scope plan changed the action incorrectly or claimed source inspection before review.")
			}
			if scenario.excluded {
				if len(planned.Files) != 0 {
					t.Fatal("A justified exclusion still scheduled content inspection.")
				}
				return
			}
			assertPlannedChangedRows(t, planned, file)
		})
	}
	for _, action := range []domain.FileReviewAction{domain.FileActionGap, domain.FileActionInspectRelated} {
		for _, requirement := range []domain.ContentReviewRequirement{domain.ContentReviewRequired, domain.ContentReviewUncertain} {
			t.Run(string(action)+"_"+string(requirement), func(t *testing.T) {
				vault := classificationGitFile(t, "credentials.vault", "$ANSIBLE_VAULT;1.1;AES256\n61616161\n", "$ANSIBLE_VAULT;1.1;AES256\n62626262\n")
				assessment := classificationAssessment(t, vault, policy, requirement, action, reviewrules.ContentEncrypted)
				assessment.Reason = "The encrypted permission changes require evidence about authorization behavior."
				assessment.Reviewed = true
				planned, decisions, err := review.PlanFileContentReview(diff.ReviewInput{Files: []diff.FileContext{vault}}, []domain.FileAssessment{assessment}, policy)
				if err != nil {
					t.Fatal(err)
				}
				assertPlannedChangedRows(t, planned, vault)
				if decisions[0].Action != action || decisions[0].Reviewed {
					t.Fatal("Reading ciphertext changed an unresolved authorization assessment into completed content review.")
				}
				var summary review.Summary
				summary.Decision = domain.ReviewDecisionComment
				summary.ApprovalWithheld = true
				summary.FileAssessments = decisions
				summary.FileAssessmentMaximumBytes = policy.FileAssessmentMaximumBytes()
				summary.FileAssessmentsDisplay = reviewrules.FindingsCollapsed
				body := review.RenderBody(summary)
				_, encoded, found := strings.Cut(body, "<!-- pr-review-agent:file-assessments:v1 ")
				if !found {
					t.Fatal("The published summary omitted its assessment record.")
				}
				encoded, _, _ = strings.Cut(encoded, " -->")
				payload, err := base64.RawURLEncoding.DecodeString(encoded)
				if err != nil {
					t.Fatal(err)
				}
				var restored []domain.FileAssessment
				if err := json.Unmarshal(payload, &restored); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(body, "| Review scope assessed | no |") || len(restored) != 1 || restored[0].ContentReviewRequirement != requirement || restored[0].Action != action {
					t.Fatal("The published scope record lost the required authorization gap.")
				}
			})
		}
	}
}

func TestFileScopePlannerRejectsMissingOrInvalidRequirement(t *testing.T) {
	file := classificationGitFile(t, "source.txt", "before\n", "after\n")
	policy := policytest.Load(t)
	for _, requirement := range []domain.ContentReviewRequirement{"", "optional"} {
		assessment := classificationAssessment(t, file, policy, requirement, domain.FileActionExclude, reviewrules.ContentData)
		_, _, err := review.PlanFileContentReview(diff.ReviewInput{Files: []diff.FileContext{file}}, []domain.FileAssessment{assessment}, policy)
		if err == nil {
			t.Fatal("The scope planner accepted a missing or invalid content review requirement.")
		}
	}
}

func classificationAssessment(t *testing.T, file diff.FileContext, policy reviewrules.Policy, requirement domain.ContentReviewRequirement, action domain.FileReviewAction, kind reviewrules.ContentKind) domain.FileAssessment {
	t.Helper()
	model := openai.NewClient(config.Config{ReviewPolicy: policy, MinimumImportance: 7, ReviewModel: "configured-model"}, nil)
	prompt, err := review.BuildFileAssessmentPrompt(model, githubapp.PullRequest{}, []diff.FileContext{file}, file.Path, policy, config.MaximumPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	_, identity, found := strings.Cut(prompt, "File identity: ")
	if !found {
		t.Fatal("The actual assessment request omitted the current file identity.")
	}
	identity, _, _ = strings.Cut(identity, "\n")
	_, evidence, found := strings.Cut(prompt, "Evidence ID: ")
	if !found {
		t.Fatal("The actual assessment request omitted the file evidence reference.")
	}
	evidence, _, _ = strings.Cut(evidence, "\n")
	return domain.FileAssessment{ContentReviewRequirement: requirement, Path: file.Path, Identity: identity, ContentKind: kind, Action: action, Reason: "The supplied change requires the selected review scope.", Evidence: []string{evidence}}
}

func assertPlannedChangedRows(t *testing.T, input diff.ReviewInput, file diff.FileContext) {
	t.Helper()
	chunks, err := diff.ChunkInput(input, config.MaximumPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int]bool)
	for _, chunk := range chunks {
		if !chunk.CoverageComplete {
			t.Fatal("The planner did not divide reviewable source into complete chunks.")
		}
		rows, err := diff.ChangedSourceRows(chunk)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			_, changed := file.ChangedRightLines[row.RightLine]
			if row.Path != file.Path || seen[row.RightLine] || !changed {
				t.Fatal("The planned source duplicated or invented a changed row.")
			}
			seen[row.RightLine] = true
		}
	}
	if len(seen) != len(file.ChangedRightLines) {
		t.Fatal("The scope decision omitted necessary changed source.")
	}
}

func classificationGitFile(t *testing.T, name, before, after string) diff.FileContext {
	t.Helper()
	directory := t.TempDir()
	command := func(arguments ...string) string {
		t.Helper()
		process := exec.CommandContext(t.Context(), "git", arguments...)
		process.Dir = directory
		output, err := process.CombinedOutput()
		if err != nil {
			t.Fatalf("Git %s failed: %v\n%s", arguments[0], err, output)
		}
		return string(output)
	}
	command("init", "--quiet")
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	command("add", name)
	command("-c", "user.name=Integration Test", "-c", "user.email=integration@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "-m", "Create classification source\n\nCo-authored-by: Codex <noreply@openai.com>")
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := command("diff", "--unified=0", "--", name)
	lines, hunks, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatal(err)
	}
	return diff.FileContext{Path: name, Status: "modified", Patch: patch, CurrentContent: after, ChangedRightLines: lines, ChangedRightHunks: hunks, CoverageComplete: true}
}

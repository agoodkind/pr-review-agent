package review_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestFileScopePlanReportsSafeValidationCategories(t *testing.T) {
	file := classificationGitFile(t, "source.txt", "before\n", "after\n")
	policy := policytest.Load(t)
	base := classificationAssessment(t, file, policy, domain.ContentReviewRequired, domain.FileActionReview, reviewrules.ContentSource)
	const privateValue = "private-model-value-must-not-appear"
	for _, scenario := range []struct {
		name     string
		code     review.AssessmentValidationCode
		observed int
		limit    int
		change   func(*domain.FileAssessment)
	}{
		{name: "contract", code: review.AssessmentContract, limit: 1, change: func(value *domain.FileAssessment) { value.ContentReviewRequirement = "" }},
		{name: "target", code: review.AssessmentTarget, limit: 1, change: func(value *domain.FileAssessment) { value.Path = privateValue }},
		{name: "identity", code: review.AssessmentIdentity, limit: 1, change: func(value *domain.FileAssessment) { value.Identity = privateValue }},
		{name: "reason bytes", code: review.AssessmentReasonLimit, observed: policy.FileAssessmentMaximumReasonBytes() + 1, limit: policy.FileAssessmentMaximumReasonBytes(), change: func(value *domain.FileAssessment) {
			value.Reason = strings.Repeat("x", policy.FileAssessmentMaximumReasonBytes()+1)
		}},
		{name: "evidence count", code: review.AssessmentEvidenceLimit, observed: policy.FileAssessmentMaximumEvidenceReferences() + 1, limit: policy.FileAssessmentMaximumEvidenceReferences(), change: func(value *domain.FileAssessment) {
			for len(value.Evidence) <= policy.FileAssessmentMaximumEvidenceReferences() {
				value.Evidence = append(value.Evidence, value.Evidence[0])
			}
		}},
		{name: "own evidence", code: review.AssessmentFileEvidence, limit: 1, change: func(value *domain.FileAssessment) { value.Evidence = []string{"pull_request"} }},
		{name: "unknown evidence", code: review.AssessmentUnknownEvidence, observed: 1, change: func(value *domain.FileAssessment) { value.Evidence = append(value.Evidence, privateValue) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assessment := base
			assessment.Evidence = slices.Clone(base.Evidence)
			scenario.change(&assessment)
			_, _, err := review.PlanFileContentReview(diff.ReviewInput{Files: []diff.FileContext{file}}, []domain.FileAssessment{assessment}, policy)
			var rejection *review.AssessmentValidationError
			if !errors.Is(err, review.ErrAssessmentIncomplete) || !errors.As(err, &rejection) || rejection.Code != scenario.code || rejection.Observed != scenario.observed || rejection.Limit != scenario.limit {
				t.Fatalf("The caller must receive the exact safe validation category and counts: %v.", err)
			}
			if strings.Contains(err.Error(), privateValue) {
				t.Fatal("Validation diagnostics exposed model content.")
			}
		})
	}
	limited := assessmentPolicyStorageLimit(t, 128)
	assessment := classificationAssessment(t, file, limited, domain.ContentReviewRequired, domain.FileActionReview, reviewrules.ContentSource)
	_, _, err := review.PlanFileContentReview(diff.ReviewInput{Files: []diff.FileContext{file}}, []domain.FileAssessment{assessment}, limited)
	var rejection *review.AssessmentValidationError
	if !errors.As(err, &rejection) || !errors.Is(err, review.ErrAssessmentIncomplete) || rejection.Code != review.AssessmentPersistenceLimit || rejection.Observed <= rejection.Limit || rejection.Limit != 128 {
		t.Fatalf("The caller must receive the actual serialized record size and configured persistence limit: %v.", err)
	}
}

func TestIncompleteClassificationRetainsUnknownScopeAfterContentInspection(t *testing.T) {
	file := classificationGitFile(t, "values.txt", "earlier payload\n", "current opaque payload\n")
	policy := policytest.Load(t)
	assessment, err := review.UnresolvedFileAssessment(file, policy)
	if err != nil {
		t.Fatal(err)
	}
	assessment.Reviewed = true
	planned, decisions, err := review.PlanFileContentReview(diff.ReviewInput{Files: []diff.FileContext{file}}, []domain.FileAssessment{assessment}, policy)
	if err != nil {
		t.Fatal(err)
	}
	assertPlannedChangedRows(t, planned, file)
	if len(decisions) != 1 || decisions[0].ContentKind != reviewrules.ContentUnknown || decisions[0].ContentReviewRequirement != domain.ContentReviewUncertain || decisions[0].Action != domain.FileActionGap || decisions[0].Reviewed || decisions[0].PermitsContentExclusion() {
		t.Fatal("Physical source inspection must not complete an unresolved contextual classification.")
	}
	var summary review.Summary
	summary.FileAssessments = decisions
	summary.FileAssessmentMaximumBytes = policy.FileAssessmentMaximumBytes()
	summary.FileAssessmentsDisplay = reviewrules.FindingsCollapsed
	if !strings.Contains(review.RenderBody(summary), "| Review scope assessed | no |") {
		t.Fatal("The published result must retain the unresolved scope assessment.")
	}
}

func TestStructuredAssessmentContractErrorsRetainTheirType(t *testing.T) {
	file := classificationGitFile(t, "source.txt", "before\n", "after\n")
	policy := policytest.Load(t)
	assessment := classificationAssessment(t, file, policy, domain.ContentReviewRequired, domain.FileActionReview, reviewrules.ContentSource)
	for _, scenario := range []struct {
		name       string
		field      string
		code       review.AssessmentValidationCode
		duplicates bool
	}{
		{name: "missing requirement", field: "content_review_requirement", code: review.AssessmentContract},
		{name: "duplicate decision", field: "duplicate_path", code: review.AssessmentDuplicate, duplicates: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			decisions := []domain.FileAssessment{assessment}
			if scenario.duplicates {
				decisions = append(decisions, assessment)
			} else {
				decisions[0].ContentReviewRequirement = ""
			}
			data, err := json.Marshal(domain.ReviewResult{FileAssessments: decisions, Findings: []domain.Finding{}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = domain.UnmarshalReviewResult(data, policy.Catalog(), nil)
			var contract *domain.FileAssessmentValidationError
			if !errors.As(err, &contract) || contract.Field != scenario.field {
				t.Fatalf("The public decoder must preserve the safe assessment field category: %v.", err)
			}
			var decoded domain.ReviewResult
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			_, _, err = review.PlanFileContentReview(diff.ReviewInput{Files: []diff.FileContext{file}}, decoded.FileAssessments, policy)
			var diagnostic *review.AssessmentValidationError
			if !errors.Is(err, review.ErrAssessmentIncomplete) || !errors.As(err, &diagnostic) || diagnostic.Code != scenario.code {
				t.Fatalf("The public planner must preserve the incomplete-assessment diagnostic: %v.", err)
			}
			if !scenario.duplicates && (!errors.As(err, &contract) || diagnostic.Field != scenario.field) {
				t.Fatal("The normalized diagnostic must preserve its original typed contract error.")
			}
		})
	}
	invalidFinding := domain.Finding{RuleID: policy.Catalog().IDs()[0], Importance: 7}
	data, err := json.Marshal(domain.ReviewResult{FileAssessments: []domain.FileAssessment{assessment}, Findings: []domain.Finding{invalidFinding}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = domain.UnmarshalReviewResult(data, policy.Catalog(), nil)
	var contract *domain.FileAssessmentValidationError
	if err == nil || errors.As(err, &contract) || errors.Is(err, review.ErrAssessmentIncomplete) {
		t.Fatal("An invalid finding must remain distinct from an incomplete file assessment.")
	}
}

func assessmentPolicyStorageLimit(t *testing.T, maximum int) reviewrules.Policy {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "review-prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prompts map[string]json.RawMessage
	if err := json.Unmarshal(data, &prompts); err != nil {
		t.Fatal(err)
	}
	var settings reviewrules.FileClassificationPolicy
	if err := json.Unmarshal(prompts["file_classification"], &settings); err != nil {
		t.Fatal(err)
	}
	settings.MaximumPersistedBytes = maximum
	prompts["file_classification"], err = json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(prompts)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "review-prompts.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := reviewrules.LoadPolicy(filepath.Join("..", "..", "config", "review-rules.json"), path)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

package review

import (
	"slices"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// PlanFileContentReview validates scope decisions before selecting source for content review.
// Conflicting exclusions become explicit gaps in the returned assessments.
// The plan retains their source for content review and reports the unresolved requirement.
func PlanFileContentReview(input diff.ReviewInput, assessments []domain.FileAssessment, policy reviewrules.Policy) (diff.ReviewInput, []domain.FileAssessment, error) {
	decisions := slices.Clone(assessments)
	var shortfall structuralShortfall
	for index := range decisions {
		assessment := &decisions[index]
		if err := assessment.Validate(); err != nil {
			return input, nil, normalizeFileAssessmentError(err)
		}
		assessment.Reviewed = false
		file := BuildFileIndex(input.Files)[assessment.Path]
		if assessment.Action == domain.FileActionExclude && (policy.RequiresFileContentReview(assessment.Path) ||
			(file.Attributes["linguist-generated"] == "false" && assessment.ContentKind == reviewrules.ContentGenerated)) {
			var data reviewrules.PromptData
			reason, err := renderPrompt(policy, "classification.required", data)
			if err != nil {
				return input, nil, err
			}
			assessment.Action = domain.FileActionReview
			assessment.ContentReviewRequirement = domain.ContentReviewRequired
			assessment.Reason = reason
		}
		if assessment.Action == domain.FileActionExclude && !assessment.PermitsContentExclusion() {
			var data reviewrules.PromptData
			reason, err := renderPrompt(policy, "coverage.requirement_conflict", data)
			if err != nil {
				return input, nil, err
			}
			assessment.Action = domain.FileActionGap
			assessment.Reason = reason
		}
		shortfall.Hunks = append(shortfall.Hunks, unreadHunk{Path: assessment.Path, Header: "", Reason: assessment.Reason, Target: nil})
	}
	result := domain.ReviewResult{Overview: "", OmissionsAcceptable: false, DecisionReason: "", Findings: nil, FileAssessments: decisions}
	validated, _, err := validateFileAssessmentsDetailed(result, shortfall, input.Files, policy)
	if err != nil {
		return input, nil, err
	}
	planned := input
	planned.Files = make([]diff.FileContext, 0, len(input.Files))
	for _, file := range input.Files {
		assessment, excluded := preflightExclusion(validated, file.Path)
		if excluded && policy.AllowsContentExclusion(assessment.ContentKind) {
			continue
		}
		planned.Files = append(planned.Files, file)
	}
	return planned, validated, nil
}

package domain

import (
	"strings"

	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// FileAssessmentValidationError identifies an invalid contract field without its value.
type FileAssessmentValidationError struct {
	Field string
}

// Error returns the field identifier without model content.
func (failure *FileAssessmentValidationError) Error() string {
	return "invalid file assessment field: " + failure.Field
}

// FileReviewAction selects necessary work independently of the content category.
type FileReviewAction string

// ContentReviewRequirement records whether changed content needs inspection.
type ContentReviewRequirement string

const (
	// ContentReviewRequired requires inspection of relevant changed behavior.
	ContentReviewRequired ContentReviewRequirement = "required"
	// ContentReviewUnnecessary permits a justified content exclusion.
	ContentReviewUnnecessary ContentReviewRequirement = "unnecessary"
	// ContentReviewUncertain requires further evidence before excluding content.
	ContentReviewUncertain ContentReviewRequirement = "uncertain"
)

const (
	// FileActionReview requires inspection of the changed content.
	FileActionReview FileReviewAction = "review"
	// FileActionInspectRelated requires evidence from related source or validation.
	FileActionInspectRelated FileReviewAction = "inspect_related"
	// FileActionExclude permits omitting content inspection after contextual assessment.
	FileActionExclude FileReviewAction = "exclude"
	// FileActionGap records an assessment that lacks necessary evidence.
	FileActionGap FileReviewAction = "gap"
)

// FileAssessment records why one changed file requires or permits content inspection.
type FileAssessment struct {
	ContentReviewRequirement ContentReviewRequirement `json:"content_review_requirement"`
	Reviewed                 bool                     `json:"reviewed,omitempty"`
	Path                     string                   `json:"path"`
	Identity                 string                   `json:"identity"`
	ContentKind              reviewrules.ContentKind  `json:"content_kind"`
	Action                   FileReviewAction         `json:"action"`
	Reason                   string                   `json:"reason"`
	Evidence                 []string                 `json:"evidence"`
}

// Validate checks the assessment contract before repository evidence validation.
func (assessment FileAssessment) Validate() error {
	if strings.TrimSpace(assessment.Path) == "" {
		return &FileAssessmentValidationError{Field: "path"}
	}
	if strings.TrimSpace(assessment.Identity) == "" {
		return &FileAssessmentValidationError{Field: "identity"}
	}
	if strings.TrimSpace(assessment.Reason) == "" {
		return &FileAssessmentValidationError{Field: "reason"}
	}
	switch assessment.ContentReviewRequirement {
	case ContentReviewRequired, ContentReviewUnnecessary, ContentReviewUncertain:
	default:
		return &FileAssessmentValidationError{Field: "content_review_requirement"}
	}
	switch assessment.ContentKind {
	case reviewrules.ContentSource, reviewrules.ContentGenerated, reviewrules.ContentEncrypted, reviewrules.ContentBinary, reviewrules.ContentDependency, reviewrules.ContentData, reviewrules.ContentExternal, reviewrules.ContentUnknown:
	default:
		return &FileAssessmentValidationError{Field: "content_kind"}
	}
	switch assessment.Action {
	case FileActionReview, FileActionInspectRelated, FileActionExclude, FileActionGap:
	default:
		return &FileAssessmentValidationError{Field: "action"}
	}
	if len(assessment.Evidence) == 0 {
		return &FileAssessmentValidationError{Field: "evidence"}
	}
	if assessment.ContentKind == reviewrules.ContentUnknown && assessment.Action == FileActionExclude {
		return &FileAssessmentValidationError{Field: "content_exclusion"}
	}
	return nil
}

// PermitsContentExclusion rejects saved assessments that do not include a review requirement.
func (assessment FileAssessment) PermitsContentExclusion() bool {
	return assessment.Action == FileActionExclude && assessment.ContentReviewRequirement == ContentReviewUnnecessary
}

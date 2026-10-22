package review

import (
	"errors"
	"fmt"

	"goodkind.io/pr-review-agent/internal/domain"
)

// AssessmentValidationCode identifies a failed check without exposing model content.
type AssessmentValidationCode string

const (
	// AssessmentMissing reports an absent required file decision.
	AssessmentMissing AssessmentValidationCode = "assessment_missing"
	// AssessmentExtra reports more decisions than the request permits.
	AssessmentExtra AssessmentValidationCode = "assessment_extra"
	// AssessmentTarget reports a decision outside the requested files.
	AssessmentTarget AssessmentValidationCode = "assessment_target"
	// AssessmentDuplicate reports repeated decisions for one file.
	AssessmentDuplicate AssessmentValidationCode = "assessment_duplicate"
	// AssessmentContract reports an invalid required assessment field.
	AssessmentContract AssessmentValidationCode = "assessment_contract"
	// AssessmentIdentity reports a decision that does not match current file evidence.
	AssessmentIdentity AssessmentValidationCode = "assessment_identity"
	// AssessmentReasonLimit reports a reason above the configured byte limit.
	AssessmentReasonLimit AssessmentValidationCode = "assessment_reason_limit"
	// AssessmentEvidenceLimit reports too many evidence references.
	AssessmentEvidenceLimit AssessmentValidationCode = "assessment_evidence_limit"
	// AssessmentFileEvidence reports a decision without its required file reference.
	AssessmentFileEvidence AssessmentValidationCode = "assessment_file_evidence"
	// AssessmentUnknownEvidence reports an unrecognized evidence reference.
	AssessmentUnknownEvidence AssessmentValidationCode = "assessment_unknown_evidence"
	// AssessmentUnsuppliedEvidence reports a reference absent from the actual request.
	AssessmentUnsuppliedEvidence AssessmentValidationCode = "assessment_unsupplied_evidence"
	// AssessmentPersistenceLimit reports a record above the configured storage limit.
	AssessmentPersistenceLimit AssessmentValidationCode = "assessment_persistence_limit"
)

// AssessmentValidationError preserves the incomplete-assessment sentinel and safe counts.
type AssessmentValidationError struct {
	Code     AssessmentValidationCode
	Field    string
	Observed int
	Limit    int
	cause    error
}

// Error returns validation identifiers and numeric counts without model content.
func (failure *AssessmentValidationError) Error() string {
	return fmt.Sprintf("file assessment validation failed: code=%s field=%s observed=%d limit=%d", failure.Code, failure.Field, failure.Observed, failure.Limit)
}

// Unwrap preserves callers that continue source review after an incomplete assessment.
func (failure *AssessmentValidationError) Unwrap() []error {
	if failure.cause != nil {
		return []error{ErrAssessmentIncomplete, failure.cause}
	}
	return []error{ErrAssessmentIncomplete}
}

func assessmentValidationFailure(code AssessmentValidationCode, observed, limit int) error {
	return &AssessmentValidationError{Code: code, Field: "", Observed: observed, Limit: limit, cause: nil}
}

func normalizeFileAssessmentError(err error) error {
	var contract *domain.FileAssessmentValidationError
	if !errors.As(err, &contract) {
		return err
	}
	code, observed := AssessmentContract, 0
	if contract.Field == "duplicate_path" {
		code, observed = AssessmentDuplicate, 2
	}
	return &AssessmentValidationError{Code: code, Field: contract.Field, Observed: observed, Limit: 1, cause: err}
}

package domain

import (
	"errors"
	"strings"
)

// MetadataFindingAssessment evaluates one saved finding against current field text.
type MetadataFindingAssessment struct {
	FindingID  string     `json:"finding_id"`
	Resolution Resolution `json:"resolution"`
	Reason     string     `json:"reason"`
	Evidence   string     `json:"evidence"`
	StartLine  int        `json:"start_line"`
	EndLine    int        `json:"end_line"`
}

// ValidateMetadataAssessments rejects invalid and duplicate finding decisions.
func ValidateMetadataAssessments(assessments []MetadataFindingAssessment) error {
	seen := make(map[string]bool, len(assessments))
	for _, assessment := range assessments {
		if strings.TrimSpace(assessment.FindingID) == "" || seen[assessment.FindingID] {
			return errors.New("metadata assessment requires a unique finding_id")
		}
		seen[assessment.FindingID] = true
		if _, err := ParseResolution(string(assessment.Resolution)); err != nil {
			return errors.New("metadata assessment resolution is invalid")
		}
		if strings.TrimSpace(assessment.Reason) == "" {
			return errors.New("metadata assessment reason is required")
		}
		if assessment.StartLine < 1 || assessment.EndLine < assessment.StartLine {
			return errors.New("metadata assessment requires current field coordinates")
		}
	}
	return nil
}

package domain

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// AssessmentDisposition records completion independently of GitHub check conclusions.
type AssessmentDisposition string

const (
	// AssessmentCompleted ends retry even when the verdict requests changes.
	AssessmentCompleted AssessmentDisposition = "completed"
	// AssessmentFailed schedules unfinished work after an operational error.
	AssessmentFailed AssessmentDisposition = "failed"
	// AssessmentIncomplete permits retry after a successful check with unread chunks.
	AssessmentIncomplete AssessmentDisposition = "incomplete"
	// AssessmentDeclined requires an explicit retry policy for refused work.
	AssessmentDeclined AssessmentDisposition = "declined"
	// AssessmentInterrupted resumes the original delivery after process shutdown.
	AssessmentInterrupted AssessmentDisposition = "interrupted"
)

// AssessmentOutcome stores recovery evidence without provider-supplied error text.
type AssessmentOutcome struct {
	Disposition      AssessmentDisposition `json:"disposition"`
	Head             HeadSHA               `json:"head"`
	MetadataRevision string                `json:"metadata_revision,omitempty"`
	FailureClasses   []string              `json:"failure_classes,omitempty"`
	AvailableAtMS    int64                 `json:"available_at_ms,omitempty"`
	CoverageComplete bool                  `json:"coverage_complete"`
	CheckRunID       int64                 `json:"check_run_id"`
	ReassessmentID   string                `json:"reassessment_id,omitempty"`
	RetryAtMS        int64                 `json:"retry_at_ms,omitempty"`
}

const assessmentOutcomePrefix = "<!-- pr-review-agent:outcome:v1 "

// EncodeAssessmentOutcome produces the hidden check record used after restart.
func EncodeAssessmentOutcome(outcome AssessmentOutcome) string {
	encoded, err := json.Marshal(outcome)
	if err != nil {
		return ""
	}
	return assessmentOutcomePrefix + base64.RawURLEncoding.EncodeToString(encoded) + " -->"
}

// DecodeAssessmentOutcome treats missing or invalid records as unknown outcomes.
func DecodeAssessmentOutcome(body string) AssessmentOutcome {
	var outcome AssessmentOutcome
	start := strings.LastIndex(body, assessmentOutcomePrefix)
	if start < 0 {
		return outcome
	}
	encoded, _, found := strings.Cut(body[start+len(assessmentOutcomePrefix):], " -->")
	if !found {
		return outcome
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return outcome
	}
	if json.Unmarshal(data, &outcome) != nil {
		var empty AssessmentOutcome
		return empty
	}
	return outcome
}

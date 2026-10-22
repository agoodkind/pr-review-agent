package reviewrules

import (
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
)

// ContentKind identifies observed content independently of the review action.
type ContentKind string

const (
	// ContentSource identifies application source or configuration.
	ContentSource ContentKind = "source"
	// ContentGenerated identifies output produced by a generator.
	ContentGenerated ContentKind = "generated"
	// ContentEncrypted identifies encrypted values.
	ContentEncrypted ContentKind = "encrypted"
	// ContentBinary identifies bytes that require a non-text inspection method.
	ContentBinary ContentKind = "binary"
	// ContentDependency identifies dependency versions and integrity information.
	ContentDependency ContentKind = "dependency"
	// ContentData identifies structured or repetitive data.
	ContentData ContentKind = "data"
	// ContentExternal identifies references to separately maintained content.
	ContentExternal ContentKind = "external"
	// ContentUnknown records content that the available evidence cannot classify.
	ContentUnknown ContentKind = "unknown"
)

// FileClassificationPolicy controls which contextual content exclusions are permitted.
type FileClassificationPolicy struct {
	MaximumPersistedBytes     int               `json:"maximum_persisted_bytes"`
	MaximumReasonBytes        int               `json:"maximum_reason_bytes"`
	MaximumEvidenceReferences int               `json:"maximum_evidence_references"`
	AllowedExclusions         []ContentKind     `json:"allowed_exclusions"`
	Instructions              string            `json:"instructions"`
	PathInstructions          map[string]string `json:"path_instructions"`
}

// Validate rejects unsupported categories and malformed path patterns.
func (policy FileClassificationPolicy) Validate() error {
	if strings.TrimSpace(policy.Instructions) == "" {
		return fmt.Errorf("file classification requires review instructions")
	}
	if policy.MaximumPersistedBytes <= 0 || policy.MaximumReasonBytes <= 0 || policy.MaximumEvidenceReferences <= 0 {
		return fmt.Errorf("file classification requires positive persistence, reason, and evidence limits")
	}
	for _, kind := range policy.AllowedExclusions {
		switch kind {
		case ContentSource, ContentGenerated, ContentEncrypted, ContentBinary, ContentDependency, ContentData, ContentExternal:
		case ContentUnknown:
			return fmt.Errorf("unknown content cannot receive a configured exclusion")
		default:
			return fmt.Errorf("file classification exclusion kind %q is invalid", kind)
		}
	}
	for pattern, instruction := range policy.PathInstructions {
		if _, err := path.Match(pattern, ""); err != nil {
			slog.Warn("File classification path pattern is invalid", "pattern", pattern, "err", err)
			return fmt.Errorf("file classification pattern %q is invalid: %w", pattern, err)
		}
		if strings.TrimSpace(instruction) == "" {
			return fmt.Errorf("file classification instruction for %q is empty", pattern)
		}
	}
	return nil
}

// FileAssessmentMaximumBytes bounds the complete persisted assessment marker.
func (policy Policy) FileAssessmentMaximumBytes() int {
	return policy.fileClassification.MaximumPersistedBytes
}

// FileAssessmentMaximumReasonBytes bounds a single diagnostic explanation.
func (policy Policy) FileAssessmentMaximumReasonBytes() int {
	return policy.fileClassification.MaximumReasonBytes
}

// FileAssessmentMaximumEvidenceReferences bounds the references in one decision.
func (policy Policy) FileAssessmentMaximumEvidenceReferences() int {
	return policy.fileClassification.MaximumEvidenceReferences
}

// AllowsContentExclusion checks the runtime category permission without granting an exclusion.
func (policy Policy) AllowsContentExclusion(kind ContentKind) bool {
	return slices.Contains(policy.fileClassification.AllowedExclusions, kind)
}

// FileReviewInstructions returns trusted scope instructions for ordinary review requests.
func (policy Policy) FileReviewInstructions() string { return policy.fileClassification.Instructions }

// FileClassificationPathInstructions returns trusted instructions for matching changed paths.
func (policy Policy) FileClassificationPathInstructions(filePath string) string {
	var output strings.Builder
	patterns := make([]string, 0, len(policy.fileClassification.PathInstructions))
	for pattern := range policy.fileClassification.PathInstructions {
		patterns = append(patterns, pattern)
	}
	slices.Sort(patterns)
	for _, pattern := range patterns {
		if matches, _ := path.Match(pattern, filePath); matches {
			output.WriteString("\n")
			output.WriteString(policy.fileClassification.PathInstructions[pattern])
		}
	}
	return output.String()
}

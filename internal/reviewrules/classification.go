package reviewrules

import (
	"fmt"
	"log/slog"
	"maps"
	"path"
	"regexp"
	"regexp/syntax"
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
	TrustedDetectors          []FileDetector    `json:"trusted_detectors"`
	PathExclusions            []FilePathRule    `json:"path_exclusions"`
	ReviewOverrides           []string          `json:"review_overrides"`
	MaximumEvidenceBytes      int               `json:"maximum_evidence_bytes"`
	MaximumPreviewBytes       int               `json:"maximum_preview_bytes"`
	MaximumDirectReviewBytes  int               `json:"maximum_direct_review_bytes"`
	UncertainContentPrefixes  []string          `json:"uncertain_content_prefixes"`
	HeaderPatterns            []string          `json:"header_patterns"`
	MaximumHeaderBytes        int               `json:"maximum_header_bytes"`
	MaximumPersistedBytes     int               `json:"maximum_persisted_bytes"`
	MaximumReasonBytes        int               `json:"maximum_reason_bytes"`
	MaximumEvidenceReferences int               `json:"maximum_evidence_references"`
	AllowedExclusions         []ContentKind     `json:"allowed_exclusions"`
	Instructions              string            `json:"instructions"`
	PathInstructions          map[string]string `json:"path_instructions"`
}

// FileDetector selects a maintained detector and its configured exclusion reason.
type FileDetector struct {
	Detector Detector    `json:"detector"`
	Kind     ContentKind `json:"kind"`
	Reason   string      `json:"reason"`
}

// Detector identifies one supported trusted content detector.
type Detector string

const (
	// DetectorGenerated uses the maintained generated-content detector.
	DetectorGenerated Detector = "generated"
	// DetectorHeader matches configured generation declarations within the bounded header.
	DetectorHeader Detector = "header"
	// DetectorVendor uses the maintained vendor path detector.
	DetectorVendor Detector = "vendor"
	// DetectorBinary uses collected binary evidence or the maintained byte detector.
	DetectorBinary Detector = "binary"
	// DetectorGeneratedAttribute reads the effective linguist-generated attribute.
	DetectorGeneratedAttribute Detector = "linguist-generated"
	// DetectorVendorAttribute reads the effective linguist-vendored attribute.
	DetectorVendorAttribute Detector = "linguist-vendored"
)

// FilePathRule matches a repository path or basename before content inspection.
type FilePathRule struct {
	Pattern string      `json:"pattern"`
	Kind    ContentKind `json:"kind"`
	Reason  string      `json:"reason"`
}

// Validate rejects unsupported categories and malformed path patterns.
func (policy FileClassificationPolicy) Validate() error {
	if err := policy.validateEvidence(); err != nil {
		return err
	}
	for _, detector := range policy.TrustedDetectors {
		switch detector.Detector {
		case DetectorGenerated, DetectorHeader, DetectorVendor, DetectorBinary, DetectorGeneratedAttribute, DetectorVendorAttribute:
		default:
			return fmt.Errorf("file classification detector %q is unsupported", detector.Detector)
		}
		if !slices.Contains(policy.AllowedExclusions, detector.Kind) || strings.TrimSpace(detector.Reason) == "" {
			return fmt.Errorf("file classification detector requires an allowed category and reason")
		}
	}
	for _, exclusion := range policy.PathExclusions {
		if !slices.Contains(policy.AllowedExclusions, exclusion.Kind) || strings.TrimSpace(exclusion.Reason) == "" {
			return fmt.Errorf("file classification path exclusion requires an allowed category and reason")
		}
		if _, err := path.Match(exclusion.Pattern, ""); err != nil {
			return fmt.Errorf("file classification path exclusion is invalid: %w", err)
		}
	}
	for _, pattern := range policy.ReviewOverrides {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("file classification review override is invalid: %w", err)
		}
	}
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

func (policy FileClassificationPolicy) validateEvidence() error {
	if slices.ContainsFunc(policy.UncertainContentPrefixes, func(prefix string) bool { return strings.TrimSpace(prefix) == "" }) {
		return fmt.Errorf("file classification uncertain content prefixes must not be empty")
	}
	if policy.MaximumEvidenceBytes <= 0 || policy.MaximumPreviewBytes <= 0 || policy.MaximumDirectReviewBytes <= 0 || policy.MaximumHeaderBytes <= 0 {
		return fmt.Errorf("file classification requires positive evidence, preview, and header limits")
	}
	for _, pattern := range policy.HeaderPatterns {
		if strings.TrimSpace(pattern) == "" {
			return fmt.Errorf("file classification header patterns must not be empty")
		}
		if _, err := syntax.Parse(pattern, syntax.Perl); err != nil {
			slog.Warn("File classification header pattern is invalid", "err", err)
			return fmt.Errorf("file classification header pattern is invalid: %w", err)
		}
	}
	return nil
}

func compileClassificationHeaders(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		matcher, err := regexp.Compile(pattern)
		if err != nil {
			slog.Warn("File classification header pattern is invalid", "err", err)
			return nil, fmt.Errorf("file classification header pattern is invalid: %w", err)
		}
		compiled = append(compiled, matcher)
	}
	return compiled, nil
}

// MatchesFileClassificationHeader evaluates the header patterns compiled during policy loading.
func (policy Policy) MatchesFileClassificationHeader(content string) bool {
	for _, pattern := range policy.fileClassificationHeaders {
		if pattern.MatchString(content) {
			return true
		}
	}
	return false
}

// FileClassification returns a separate copy of the configured preflight rules.
func (policy Policy) FileClassification() FileClassificationPolicy {
	settings := policy.fileClassification
	settings.AllowedExclusions = slices.Clone(settings.AllowedExclusions)
	settings.PathInstructions = maps.Clone(settings.PathInstructions)
	settings.TrustedDetectors = slices.Clone(settings.TrustedDetectors)
	settings.PathExclusions = slices.Clone(settings.PathExclusions)
	settings.ReviewOverrides = slices.Clone(settings.ReviewOverrides)
	settings.UncertainContentPrefixes = slices.Clone(settings.UncertainContentPrefixes)
	settings.HeaderPatterns = slices.Clone(settings.HeaderPatterns)
	return settings
}

// RequiresFileContentReview applies configured overrides before content exclusions.
func (policy Policy) RequiresFileContentReview(filePath string) bool {
	for _, pattern := range policy.fileClassification.ReviewOverrides {
		matched, _ := path.Match(pattern, filePath)
		if !strings.Contains(pattern, "/") {
			matched, _ = path.Match(pattern, path.Base(filePath))
		}
		if matched {
			return true
		}
	}
	return false
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

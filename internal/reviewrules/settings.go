package reviewrules

type classificationSettings struct {
	TrustedDetectors          []FileDetector `json:"trusted_detectors"`
	PathExclusions            []FilePathRule `json:"path_exclusions"`
	ReviewOverrides           []string       `json:"review_overrides"`
	MaximumEvidenceBytes      int            `json:"maximum_evidence_bytes"`
	MaximumPreviewBytes       int            `json:"maximum_preview_bytes"`
	MaximumDirectReviewBytes  int            `json:"maximum_direct_review_bytes"`
	UncertainContentPrefixes  []string       `json:"uncertain_content_prefixes"`
	HeaderPatterns            []string       `json:"header_patterns"`
	MaximumHeaderBytes        int            `json:"maximum_header_bytes"`
	MaximumPersistedBytes     int            `json:"maximum_persisted_bytes"`
	MaximumReasonBytes        int            `json:"maximum_reason_bytes"`
	MaximumEvidenceReferences int            `json:"maximum_evidence_references"`
	AllowedExclusions         []ContentKind  `json:"allowed_exclusions"`
}

func (settings classificationSettings) policy(
	prompts classificationPrompts,
) FileClassificationPolicy {
	return FileClassificationPolicy{
		TrustedDetectors:          settings.TrustedDetectors,
		PathExclusions:            settings.PathExclusions,
		ReviewOverrides:           settings.ReviewOverrides,
		MaximumEvidenceBytes:      settings.MaximumEvidenceBytes,
		MaximumPreviewBytes:       settings.MaximumPreviewBytes,
		MaximumDirectReviewBytes:  settings.MaximumDirectReviewBytes,
		UncertainContentPrefixes:  settings.UncertainContentPrefixes,
		HeaderPatterns:            settings.HeaderPatterns,
		MaximumHeaderBytes:        settings.MaximumHeaderBytes,
		MaximumPersistedBytes:     settings.MaximumPersistedBytes,
		MaximumReasonBytes:        settings.MaximumReasonBytes,
		MaximumEvidenceReferences: settings.MaximumEvidenceReferences,
		AllowedExclusions:         settings.AllowedExclusions,
		Instructions:              prompts.Instructions,
		PathInstructions:          prompts.PathInstructions,
	}
}

package review

import (
	"path"
	"strings"
	"unicode/utf8"

	"github.com/go-enry/go-enry/v2"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// PlanFileAssessments assesses trusted evidence before selecting unresolved files.
func PlanFileAssessments(input diff.ReviewInput, policy reviewrules.Policy) ([]domain.FileAssessment, []diff.FileContext, error) {
	var decisions []domain.FileAssessment
	var unresolved []diff.FileContext
	for _, file := range input.Files {
		kind, reason, excluded := deterministicExclusion(file, policy)
		if !excluded {
			if !clearReviewableContent(file, policy) {
				unresolved = append(unresolved, file)
				continue
			}
			kind = reviewrules.ContentSource
			var data reviewrules.PromptData
			var err error
			reason, err = renderPrompt(policy, "classification.required", data)
			if err != nil {
				return nil, nil, err
			}
		}
		assessment := domain.FileAssessment{
			Path: file.Path, Identity: assessmentIdentity(file, policy), ContentKind: kind,
			ContentReviewRequirement: domain.ContentReviewRequired, Action: domain.FileActionReview,
			Reason: reason, Evidence: []string{assessmentEvidenceID(file, policy)}, Reviewed: false,
		}
		if excluded {
			assessment.ContentReviewRequirement = domain.ContentReviewUnnecessary
			assessment.Action = domain.FileActionExclude
		}
		decisions = append(decisions, assessment)
	}
	validated, err := normalizeFileAssessments(input, decisions, policy)
	return validated, unresolved, err
}

func clearReviewableContent(file diff.FileContext, policy reviewrules.Policy) bool {
	settings := policy.FileClassification()
	content := file.ClassificationContent()
	if file.Gap != diff.CoverageGapNone || !utf8.ValidString(content) || enry.IsBinary([]byte(content)) {
		return false
	}
	if file.IsSubmodule() {
		return true
	}
	if len(file.Patch) > settings.MaximumDirectReviewBytes {
		return false
	}
	for _, prefix := range settings.UncertainContentPrefixes {
		if strings.HasPrefix(strings.TrimSpace(content), prefix) {
			return false
		}
	}
	return policy.RequiresFileContentReview(file.Path) || enry.GetLanguage(file.Path, []byte(content)) != ""
}

func deterministicExclusion(file diff.FileContext, policy reviewrules.Policy) (reviewrules.ContentKind, string, bool) {
	settings := policy.FileClassification()
	if policy.RequiresFileContentReview(file.Path) {
		return "", "", false
	}
	// Retrieval failures require their own assessment even when a path matches an exclusion.
	if file.Gap != diff.CoverageGapNone && file.Gap != diff.CoverageGapBinary {
		return "", "", false
	}
	generatedSuppressed := file.Attributes["linguist-generated"] == "false"
	content := file.ClassificationContent()
	for _, detector := range settings.TrustedDetectors {
		if generatedSuppressed && detector.Kind == reviewrules.ContentGenerated {
			continue
		}
		matched := false
		switch detector.Detector {
		case reviewrules.DetectorGenerated:
			matched = enry.IsGenerated(file.Path, []byte(content))
		case reviewrules.DetectorHeader:
			matched = policy.MatchesFileClassificationHeader(truncateUTF8(content, settings.MaximumHeaderBytes))
		case reviewrules.DetectorVendor:
			matched = file.Attributes["linguist-vendored"] != "false" && enry.IsVendor(file.Path)
		case reviewrules.DetectorBinary:
			matched = file.Gap == diff.CoverageGapBinary || enry.IsBinary([]byte(content))
		case reviewrules.DetectorGeneratedAttribute, reviewrules.DetectorVendorAttribute:
			matched = file.Attributes[string(detector.Detector)] == "true"
		}
		if matched {
			return detector.Kind, detector.Reason, true
		}
	}
	for _, exclusion := range settings.PathExclusions {
		if generatedSuppressed && exclusion.Kind == reviewrules.ContentGenerated {
			continue
		}
		if classificationPathMatches(exclusion.Pattern, file.Path) {
			return exclusion.Kind, exclusion.Reason, true
		}
	}
	return "", "", false
}

func classificationPathMatches(pattern, filePath string) bool {
	if strings.Contains(pattern, "/") {
		matched, _ := path.Match(pattern, filePath)
		return matched
	}
	matched, _ := path.Match(pattern, path.Base(filePath))
	return matched
}

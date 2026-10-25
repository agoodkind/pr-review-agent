package review

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

const fileAssessmentMarkerPrefix = "<!-- pr-review-agent:file-assessments:v1 "

func assessmentPersistenceReason(assessments []domain.FileAssessment, policy reviewrules.Policy) (string, error) {
	if len(assessments) == 0 || encodeFileAssessments(assessments, policy.FileAssessmentMaximumBytes()) != "" {
		return "", nil
	}
	var data reviewrules.PromptData
	return renderPrompt(policy, "coverage.persistence_limit", data)
}

func assessmentDecisionReason(assessments []domain.FileAssessment) string {
	for _, assessment := range assessments {
		if assessment.Action != "exclude" {
			return assessment.Reason
		}
	}
	if len(assessments) == 1 {
		return assessments[0].Reason
	}
	return ""
}

func assessmentIdentity(file diff.FileContext, policy reviewrules.Policy) string {
	digest := sha256.Sum256([]byte(policy.Fingerprint() + "\x00" + file.Path + "\x00" + file.Status + "\x00" + file.Patch + "\x00" + file.CurrentContent + "\x00" + string(file.Gap) + "\x00" + classificationAttributes(file)))
	return hex.EncodeToString(digest[:])
}

func assessmentEvidenceID(file diff.FileContext, policy reviewrules.Policy) string {
	return "file:" + assessmentIdentity(file, policy)
}

func assessmentPaths(shortfall structuralShortfall) []string {
	paths := make([]string, 0)
	for _, hunk := range shortfall.Hunks {
		if hunk.Target == nil && !slices.Contains(paths, hunk.Path) {
			paths = append(paths, hunk.Path)
		}
	}
	slices.Sort(paths)
	return paths
}

func validateFileAssessments(result domain.ReviewResult, shortfall structuralShortfall, files []diff.FileContext, policy reviewrules.Policy) ([]domain.FileAssessment, bool) {
	validated, accepted, _ := validateFileAssessmentsDetailed(result, shortfall, files, policy)
	return validated, accepted
}

func validateFileAssessmentsDetailed(result domain.ReviewResult, shortfall structuralShortfall, files []diff.FileContext, policy reviewrules.Policy) ([]domain.FileAssessment, bool, error) {
	paths := assessmentPaths(shortfall)
	index := BuildFileIndex(files)
	accepted := true
	for _, hunk := range shortfall.Hunks {
		if hunk.Target != nil {
			accepted = result.OmissionsAcceptable
		}
	}
	validated := make([]domain.FileAssessment, 0, len(paths))
	seen := make(map[string]bool)
	for _, assessment := range result.FileAssessments {
		file, exists := index[assessment.Path]
		if !exists || !slices.Contains(paths, assessment.Path) {
			return nil, false, assessmentValidationFailure(AssessmentTarget, 0, 1)
		}
		if seen[assessment.Path] {
			return nil, false, assessmentValidationFailure(AssessmentDuplicate, 2, 1)
		}
		if err := validateAssessmentFields(assessment, file, files, policy); err != nil {
			return nil, false, err
		}
		seen[assessment.Path] = true
		assessment.Reason = sanitizeDecisionReason(assessment.Reason)
		reviewed := assessment.Action == domain.FileActionReview && assessment.Reviewed
		excluded := assessment.PermitsContentExclusion() && !assessment.Reviewed && policy.AllowsContentExclusion(assessment.ContentKind)
		if !reviewed && !excluded {
			accepted = false
		}
		validated = append(validated, assessment)
	}
	if len(validated) != len(paths) {
		return validated, false, assessmentValidationFailure(AssessmentMissing, len(validated), len(paths))
	}
	completed := slices.Clone(validated)
	for index := range completed {
		if completed[index].Action == domain.FileActionReview {
			completed[index].Reviewed = true
		}
	}
	if len(completed) > 0 && encodeFileAssessments(completed, policy.FileAssessmentMaximumBytes()) == "" {
		payload, err := json.Marshal(completed)
		if err != nil {
			return nil, false, assessmentValidationFailure(AssessmentContract, 0, 1)
		}
		size := len(fileAssessmentMarkerPrefix) + base64.RawURLEncoding.EncodedLen(len(payload)) + len(" -->")
		return nil, false, assessmentValidationFailure(AssessmentPersistenceLimit, size, policy.FileAssessmentMaximumBytes())
	}
	slices.SortFunc(validated, func(left, right domain.FileAssessment) int { return strings.Compare(left.Path, right.Path) })
	return validated, accepted, nil
}

func validateAssessmentFields(assessment domain.FileAssessment, file diff.FileContext, files []diff.FileContext, policy reviewrules.Policy) error {
	if err := assessment.Validate(); err != nil {
		return normalizeFileAssessmentError(err)
	}
	if assessment.Identity != assessmentIdentity(file, policy) {
		return assessmentValidationFailure(AssessmentIdentity, 0, 1)
	}
	if count := len(assessment.Reason); count > policy.FileAssessmentMaximumReasonBytes() {
		return assessmentValidationFailure(AssessmentReasonLimit, count, policy.FileAssessmentMaximumReasonBytes())
	}
	if count := len(assessment.Evidence); count > policy.FileAssessmentMaximumEvidenceReferences() {
		return assessmentValidationFailure(AssessmentEvidenceLimit, count, policy.FileAssessmentMaximumEvidenceReferences())
	}
	fileEvidence := false
	for _, reference := range assessment.Evidence {
		if reference == assessmentEvidenceID(file, policy) {
			fileEvidence = true
			continue
		}
		if reference != "pull_request" && reference != "changed_files" && !relatedEvidenceExists(reference, files, policy) {
			return assessmentValidationFailure(AssessmentUnknownEvidence, 1, 0)
		}
	}
	if !fileEvidence {
		return assessmentValidationFailure(AssessmentFileEvidence, 0, 1)
	}
	return nil
}

func (pass *chunkPass) fileWasReviewed(path string) bool {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	file, exists := BuildFileIndex(pass.work.Files)[path]
	if !exists || !file.CoverageComplete {
		return false
	}
	found := false
	for _, chunk := range pass.work.Chunks {
		if !slices.Contains(chunk.Paths, path) {
			continue
		}
		found = true
		if !chunk.CoverageComplete || !pass.reviewedChunks[chunkID(chunk)] {
			return false
		}
	}
	return found
}

func relatedEvidenceExists(reference string, files []diff.FileContext, policy reviewrules.Policy) bool {
	for _, file := range files {
		if file.ClassificationContent() != "" && reference == "related:"+assessmentIdentity(file, policy) {
			return true
		}
	}
	return false
}

func (pass *chunkPass) fileAssessments() []domain.FileAssessment {
	pass.mu.Lock()
	assessments := slices.Clone(pass.decision.assessments)
	for _, initial := range pass.work.FileAssessments {
		if !slices.ContainsFunc(assessments, func(existing domain.FileAssessment) bool { return existing.Path == initial.Path }) {
			assessments = append(assessments, initial)
		}
	}
	pass.mu.Unlock()
	for index := range assessments {
		if assessments[index].Action == domain.FileActionReview && assessments[index].ContentReviewRequirement == domain.ContentReviewRequired {
			assessments[index].Reviewed = pass.fileWasReviewed(assessments[index].Path)
		}
	}
	return assessments
}

func preflightExclusion(assessments []domain.FileAssessment, path string) (domain.FileAssessment, bool) {
	for _, assessment := range assessments {
		if assessment.Path == path && assessment.PermitsContentExclusion() {
			return assessment, true
		}
	}
	var empty domain.FileAssessment
	return empty, false
}

func (pass *chunkPass) seedFileAssessments(policy reviewrules.Policy) {
	pass.mu.Lock()
	for _, model := range pass.work.AssessmentModels {
		pass.models.add(model)
	}
	pass.requests += pass.work.AssessmentRequests
	pass.mu.Unlock()
	var excluded []domain.FileAssessment
	for _, assessment := range pass.work.FileAssessments {
		if assessment.PermitsContentExclusion() {
			excluded = append(excluded, assessment)
		}
	}
	if len(excluded) == 0 {
		return
	}
	pass.replaceOmissionDecision(domain.ReviewResult{Overview: "", OmissionsAcceptable: true, DecisionReason: assessmentDecisionReason(excluded), Findings: nil, FileAssessments: excluded}, policy)
	pass.mu.Lock()
	pass.coverage = false
	pass.mu.Unlock()
}

func encodeFileAssessments(assessments []domain.FileAssessment, maximumBytes ...int) string {
	if len(assessments) == 0 || len(maximumBytes) != 1 || maximumBytes[0] <= 0 {
		return ""
	}
	for _, assessment := range assessments {
		if assessment.Validate() != nil || (assessment.Action == domain.FileActionExclude && !assessment.PermitsContentExclusion()) {
			return ""
		}
	}
	encoded, err := json.Marshal(assessments)
	if err != nil {
		return ""
	}
	marker := fileAssessmentMarkerPrefix + base64.RawURLEncoding.EncodeToString(encoded) + " -->"
	if len(marker) > maximumBytes[0] {
		return ""
	}
	return marker
}

func decodeFileAssessments(body string) []domain.FileAssessment {
	_, payload, found := strings.Cut(body, fileAssessmentMarkerPrefix)
	if !found {
		return nil
	}
	payload, _, found = strings.Cut(payload, " -->")
	if !found {
		return nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var assessments []domain.FileAssessment
	if err := json.Unmarshal(decoded, &assessments); err != nil {
		return nil
	}
	seen := make(map[string]bool)
	for _, assessment := range assessments {
		if assessment.Validate() != nil || seen[assessment.Path] || (assessment.Action == domain.FileActionExclude && !assessment.PermitsContentExclusion()) {
			return nil
		}
		seen[assessment.Path] = true
	}
	return assessments
}

func assessmentPromptRow(file diff.FileContext, policy reviewrules.Policy) (string, error) {
	var data reviewrules.PromptData
	data.Identity = assessmentIdentity(file, policy)
	data.EvidenceID = assessmentEvidenceID(file, policy)
	data.Status = escapeOmissionPromptText(file.Status, policy)
	data.ContentBytes = len(file.ClassificationContent())
	data.Attributes = escapeOmissionPromptText(truncateUTF8(classificationAttributes(file), policy.FileClassification().MaximumPreviewBytes), policy)
	return renderPrompt(policy, "classification.file_evidence", data)
}

func classificationAttributes(file diff.FileContext) string {
	keys := make([]string, 0, len(file.Attributes))
	for key := range file.Attributes {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var attributes strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&attributes, "%s=%s\n", key, file.Attributes[key])
	}
	return attributes.String()
}

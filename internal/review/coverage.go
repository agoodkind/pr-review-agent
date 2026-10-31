package review

// This file is what a run leaves behind when part of the head is beyond what
// this service can read at all.
//
// A shortfall has two kinds and they need different handling. A chunk whose
// model call or comment post failed is temporary, so it stays pending. A hunk
// larger than one model request, a binary file, and a patch GitHub will not
// supply come back identically on every later run, so the model decides them
// from the current pull request and every signal that is available.
//
// The model receives metadata for every structural shortfall. When that
// metadata is insufficient for approval, the run withholds approval and explains
// why. Requested changes require an actionable inline finding. A model call that
// does not complete leaves the verdict undecided.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
	"goodkind.io/pr-review-agent/internal/runlog"
)

// unreadHunk names one piece of the head nobody read, in the terms a reader can
// go and look at: the file, the hunk inside it, and why it was not read.
type unreadHunk struct {
	Path   string                `json:"path"`
	Header string                `json:"header"`
	Reason string                `json:"reason"`
	Target *domain.FindingTarget `json:"target,omitempty"`
}

const omissionMarkerPrefix = "<!-- pr-review-agent:omissions:v1 "

const omissionDecisionMarkerPrefix = "<!-- pr-review-agent:omissions-acceptable:v1 "

const decisionReasonMarkerPrefix = "<!-- pr-review-agent:decision-reason:v1 "

const maximumPullRequestDescriptionBytes = 8000

// structuralShortfall is everything one delta holds that no later run can read.
type structuralShortfall struct {
	Hunks []unreadHunk
}

type omissionDecision struct {
	assessments []domain.FileAssessment
	chunk       int
	acceptable  bool
	reason      string
	recorded    bool
	complete    bool
}

func (pass *chunkPass) decidedOmissions() bool {
	structural := pass.structuralShortfall().present()
	pass.mu.Lock()
	defer pass.mu.Unlock()
	return pass.votes > 0 && (!structural || pass.decision.complete)
}

func (pass *chunkPass) acceptsOmissions() bool {
	structural := pass.structuralShortfall().present()
	pass.mu.Lock()
	defer pass.mu.Unlock()
	return pass.votes > 0 && pass.omissionsOK &&
		(!structural || pass.decision.complete)
}

func (pass *chunkPass) recordOmissionDecision(chunk int, result domain.ReviewResult, policies ...reviewrules.Policy) {
	for index := range result.FileAssessments {
		result.FileAssessments[index].Reviewed = false
	}
	var policy reviewrules.Policy
	if len(policies) > 0 {
		policy = policies[0]
	}
	shortfall := pass.structuralShortfall()
	assessments, accepted := validateFileAssessments(result, shortfall, pass.work.Files, policy)
	pass.mu.Lock()
	defer pass.mu.Unlock()
	pass.votes++
	if !accepted {
		pass.omissionsOK = false
	}
	candidate := omissionDecision{
		assessments: assessments,
		chunk:       chunk,
		acceptable:  accepted,
		reason:      scopeDecisionReason(result.DecisionReason, shortfall.Hunks, assessments),
		recorded:    true,
		complete:    false,
	}
	preferRejected := !candidate.acceptable && pass.decision.acceptable
	sameDecision := candidate.acceptable == pass.decision.acceptable
	preferReason := sameDecision && pass.decision.reason == "" && candidate.reason != ""
	sameReasonState := (candidate.reason == "") == (pass.decision.reason == "")
	preferEarlier := sameDecision && sameReasonState &&
		chunk < pass.decision.chunk
	if !pass.decision.recorded || preferRejected || preferReason || preferEarlier {
		pass.decision = candidate
	}
}

// replaceOmissionDecision records the answer that saw every omission. Earlier
// chunk answers could not include a hunk that became unread during their calls.
func (pass *chunkPass) replaceOmissionDecision(result domain.ReviewResult, policies ...reviewrules.Policy) {
	var policy reviewrules.Policy
	if len(policies) > 0 {
		policy = policies[0]
	}
	shortfall := pass.structuralShortfall()
	assessments, accepted := validateFileAssessments(result, shortfall, pass.work.Files, policy)
	pass.mu.Lock()
	defer pass.mu.Unlock()
	pass.votes = 1
	pass.omissionsOK = accepted
	pass.decision = omissionDecision{
		assessments: assessments,
		chunk:       0,
		acceptable:  accepted,
		reason:      scopeDecisionReason(result.DecisionReason, shortfall.Hunks, assessments),
		recorded:    true,
		complete:    true,
	}
}

func (pass *chunkPass) decisionReason() string {
	shortfall := pass.structuralShortfall()
	pass.mu.Lock()
	defer pass.mu.Unlock()
	return scopeDecisionReason(pass.decision.reason, shortfall.Hunks, pass.decision.assessments)
}

func scopeDecisionReason(reason string, omissions []unreadHunk, assessments []domain.FileAssessment) string {
	if len(omissions) > 0 {
		return sanitizeDecisionReason(reason)
	}
	for _, assessment := range assessments {
		if assessment.Validate() != nil {
			continue
		}
		switch assessment.Action {
		case domain.FileActionGap, domain.FileActionInspectRelated:
			return sanitizeDecisionReason(reason)
		case domain.FileActionExclude:
			if assessment.PermitsContentExclusion() {
				return sanitizeDecisionReason(reason)
			}
		case domain.FileActionReview:
		}
	}
	return ""
}

func sanitizeDecisionReason(reason string) string {
	reason = strings.Join(strings.Fields(sanitizeProse(reason)), " ")
	reason = strings.ReplaceAll(reason, "<!--", "&lt;!--")
	return strings.ReplaceAll(reason, "-->", "--&gt;")
}

// present reports whether this delta holds anything unreadable at all.
func (shortfall structuralShortfall) present() bool {
	return len(shortfall.Hunks) > 0
}

// paths names the files involved, for the log line.
func (shortfall structuralShortfall) paths() []string {
	paths := make([]string, 0, len(shortfall.Hunks))
	for _, hunk := range shortfall.Hunks {
		paths = append(paths, hunk.sourceLabel())
	}
	return paths
}

// Reasons a piece of the head went unread, in this service's own words. Each
// one is published on the pull request, so none of them quotes anything this
// service did not write.
const (
	oversizedHunkReason   = "larger than one model request allows"
	binaryFileReason      = "a binary file, which carries no reviewable patch"
	patchAbsentReason     = "GitHub supplied no patch for this file"
	patchUnreadableReason = "the patch GitHub supplied could not be read whole"
	contentMissingReason  = "GitHub will not serve this file's content at this commit"
	// truncatedAnswerReason names a hunk the model began answering and never
	// finished. A chunk whose answer runs past the completion budget is normally
	// halved and each half asked separately, so this is reached only by a chunk
	// already down to one hunk, which is the smallest unit a chunk is cut into.
	truncatedAnswerReason = "the model's answer ran past its completion budget, and a hunk cannot be split further"
)

// sortedUnreadHunks orders unread hunks by path and then by hunk. Chunks answer
// concurrently, so without this the same run could name the same hunks in a
// different order on the check run and in the comment.
func sortedUnreadHunks(hunks []unreadHunk) []unreadHunk {
	ordered := append([]unreadHunk{}, hunks...)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].sourceLabel() != ordered[right].sourceLabel() {
			return ordered[left].sourceLabel() < ordered[right].sourceLabel()
		}
		return ordered[left].Header < ordered[right].Header
	})
	return ordered
}

// classifyStructuralShortfall names every piece of this delta that will go
// unread on every later run as surely as it did on this one.
//
// It reads the delta rather than the pass. What one pass managed to do says
// nothing about whether a hunk fits in a model request, and a run that re-reads
// nothing because an earlier run already read every chunk must still reach the
// same verdict about the same oversized hunk.
func classifyStructuralShortfall(work deltaWork) structuralShortfall {
	hunks := make([]unreadHunk, 0)
	for _, assessment := range work.FileAssessments {
		if assessment.Action == domain.FileActionExclude || assessment.Action == domain.FileActionGap || assessment.Action == domain.FileActionInspectRelated {
			hunks = append(hunks, unreadHunk{Path: assessment.Path, Header: "", Reason: sanitizeDecisionReason(assessment.Reason), Target: nil})
		}
	}
	for _, file := range work.Files {
		if !file.Gap.Recurs() {
			continue
		}
		hunks = append(hunks, unreadHunk{
			Path:   file.Path,
			Header: "",
			Reason: fileGapReason(file.Gap),
			Target: nil,
		})
	}
	for _, chunk := range work.Chunks {
		for _, piece := range chunk.Pieces {
			if !piece.Oversized {
				continue
			}
			hunks = append(hunks, unreadPiece(piece, oversizedHunkReason))
		}
	}
	return structuralShortfall{Hunks: hunks}
}

// omissionPrompt gives the existing review call enough metadata to decide
// whether a structural omission prevents a reliable verdict.
func omissionPrompt(shortfall structuralShortfall, files []diff.FileContext, maximumBytes int, policy reviewrules.Policy, assessments ...[]domain.FileAssessment) (string, error) {
	var data reviewrules.PromptData
	if !shortfall.present() {
		return renderPrompt(policy, "omissions.none", data)
	}
	data.Input = policy.WrapUntrusted("")
	prefix, err := renderPrompt(policy, "omissions.input", data)
	if err != nil {
		return "", err
	}
	const omittedFormat = "Unread changes not listed: %d (the request size limit prevented listing their details).\n\n"
	settings := policy.FileClassification()
	metadataBudget := min(maximumBytes/4, settings.MaximumEvidenceBytes) - len(prefix) - 1
	hunks := sortedUnreadHunks(shortfall.Hunks)
	omittedNoticeBudget := len(fmt.Sprintf(omittedFormat, len(hunks)))
	var metadata strings.Builder
	listed := 0
	for _, hunk := range hunks {
		row := fmt.Sprintf("Source: %s\nRange: %s\nReason: %s\n\n",
			escapeOmissionPromptText(hunk.sourceLabel(), policy), escapeOmissionPromptText(hunk.Header, policy), escapeOmissionPromptText(hunk.Reason, policy))
		if file, found := BuildFileIndex(files)[hunk.Path]; found {
			evidence, err := assessmentPromptRow(file, policy)
			if err != nil {
				return "", err
			}
			row += evidence
		}
		if metadata.Len()+len(row)+omittedNoticeBudget > metadataBudget {
			break
		}
		metadata.WriteString(row)
		listed++
	}
	if listed < len(hunks) {
		fmt.Fprintf(&metadata, omittedFormat, len(hunks)-listed)
	}
	fileIndex := BuildFileIndex(files)
	seen := make(map[string]bool)
	for _, hunk := range hunks[:listed] {
		file, found := fileIndex[hunk.Path]
		if !found || file.ClassificationContent() == "" || seen[hunk.Path] || excludedPromptContent(file, policy, assessments) {
			continue
		}
		seen[hunk.Path] = true
		content := file.ClassificationContent()
		header := fmt.Sprintf("Available file content excerpt for %s (%d bytes in the file; this excerpt is not a diff and cannot anchor findings):\n", escapeOmissionPromptText(hunk.Path, policy), len(content))
		previewBudget := metadataBudget - metadata.Len() - len(header) - len("\n\n")
		if previewBudget <= 0 {
			break
		}
		preview := omissionContentPreview(content, min(settings.MaximumPreviewBytes, previewBudget), policy)
		metadata.WriteString(header)
		metadata.WriteString(preview)
		metadata.WriteString("\n\n")
	}
	for _, file := range files {
		if seen[file.Path] || file.ClassificationContent() == "" || excludedPromptContent(file, policy, assessments) {
			continue
		}
		header := fmt.Sprintf("Related evidence ID: related:%s\nChanged parent file: %s\nAvailable file content excerpt:\n", assessmentIdentity(file, policy), escapeOmissionPromptText(file.Path, policy))
		remaining := metadataBudget - metadata.Len() - len(header) - 2
		if remaining <= 0 {
			break
		}
		preview := omissionContentPreview(file.ClassificationContent(), min(settings.MaximumPreviewBytes, remaining), policy)
		metadata.WriteString(header)
		metadata.WriteString(preview)
		metadata.WriteString("\n\n")
	}
	data.Input = policy.WrapUntrusted(strings.TrimSpace(metadata.String()))
	return renderPrompt(policy, "omissions.input", data)
}

func excludedPromptContent(file diff.FileContext, policy reviewrules.Policy, decisions [][]domain.FileAssessment) bool {
	if _, _, excluded := deterministicExclusion(file, policy); excluded {
		return true
	}
	for _, assessments := range decisions {
		if _, excluded := preflightExclusion(assessments, file.Path); excluded {
			return true
		}
	}
	return false
}

func omissionContentPreview(content string, maximumBytes int, policy reviewrules.Policy) string {
	content = escapeOmissionPromptText(content, policy)
	if len(content) <= maximumBytes {
		return content
	}
	const omitted = "\n[The middle of the current file is not shown.]\n"
	if maximumBytes <= len(omitted) {
		return truncateUTF8(content, maximumBytes)
	}
	available := maximumBytes - len(omitted)
	headBytes := available / 2
	tailBytes := available - headBytes
	return truncateUTF8(content, headBytes) + omitted +
		strings.ToValidUTF8(content[len(content)-tailBytes:], "")
}

// decideUnreadableHunks asks one compact question after a chunk answer was cut
// off. The model sees the current pull request, its discussions, the readable
// summaries, and every unread hunk. Findings still require changed lines.
func (service *Service) decideUnreadableHunks(ctx context.Context, pass *chunkPass) error {
	shortfall := pass.structuralShortfall()
	if !shortfall.present() {
		return nil
	}
	var fileDecisions []domain.FileAssessment
	for _, path := range assessmentPaths(shortfall) {
		if assessment, exists := preflightExclusion(pass.work.FileAssessments, path); exists {
			fileDecisions = append(fileDecisions, assessment)
			continue
		}
		callCtx, cancel := modelRequestContext(ctx, service.model, pass.settings.chunkTimeout)
		completion, err := AssessFileContent(callCtx, service.model, pass.work.PullRequest, pass.work.Files, path, service.reviewPolicy, pass.settings.maximumPromptBytes, pass.work.FileAssessments)
		cancel()
		models := modelSet{names: nil, seen: nil}
		models.add(completion.Model)
		pass.recordCall(models, 1)
		if err != nil {
			return err
		}
		if completion.Result.FileAssessments[0].Action == domain.FileActionReview && pass.fileWasReviewed(path) {
			completion.Result.FileAssessments[0].Reviewed = true
		}
		fileDecisions = append(fileDecisions, completion.Result.FileAssessments...)
	}
	metadata := false
	for _, hunk := range shortfall.Hunks {
		metadata = metadata || hunk.Target != nil
	}
	if !metadata {
		reason := assessmentDecisionReason(fileDecisions)
		limitReason, err := assessmentPersistenceReason(fileDecisions, service.reviewPolicy)
		if err != nil {
			return err
		}
		if limitReason != "" {
			reason = limitReason
		}
		pass.replaceOmissionDecision(domain.ReviewResult{Overview: "", OmissionsAcceptable: true, DecisionReason: reason, Findings: nil, FileAssessments: fileDecisions}, service.reviewPolicy)
		return nil
	}
	return service.decideUnreadableMetadata(ctx, pass, shortfall, fileDecisions)
}

func (service *Service) decideUnreadableMetadata(ctx context.Context, pass *chunkPass, shortfall structuralShortfall, fileDecisions []domain.FileAssessment) error {
	logger := gklog.L(ctx)
	var reviewed strings.Builder
	for _, overview := range pass.overviews() {
		if value := sanitizeReportText(overview); value != "" {
			reviewed.WriteString("\n- ")
			reviewed.WriteString(value)
		}
	}
	var data reviewrules.PromptData
	instruction, err := renderPrompt(service.reviewPolicy, "omissions.decision", data)
	if err != nil {
		logger.ErrorContext(ctx, "Unread content policy rendering failed", "err", err)
		return err
	}
	omissions, err := omissionPrompt(shortfall, pass.work.Files, pass.settings.maximumPromptBytes, service.reviewPolicy, pass.work.FileAssessments)
	if err != nil {
		logger.ErrorContext(ctx, "Unread content prompt rendering failed", "err", err)
		return err
	}
	contextText, err := BuildPullRequestContext(pass.work.PullRequest, pass.work.Files, service.reviewPolicy)
	if err != nil {
		logger.ErrorContext(ctx, "Pull request prompt rendering failed", "err", err)
		return err
	}
	required := instruction + omissions
	contextText +=
		pass.disputePrompt + "The readable parts were summarized as follows:\n" +
			service.reviewPolicy.WrapUntrusted(reviewed.String())
	prompt, err := BoundReviewPrompt(service.model, required, contextText, pass.settings.maximumPromptBytes)
	if err != nil {
		return err
	}

	callCtx, cancel := modelRequestContext(ctx, service.model, pass.settings.chunkTimeout)
	defer cancel()
	completion, err := service.model.Review(callCtx, prompt)
	models := modelSet{names: nil, seen: nil}
	if completion.Model != "" {
		models.add(completion.Model)
	}
	pass.recordCall(models, 1)
	if err != nil {
		logger.ErrorContext(ctx, "decide unread changes", slog.String("err", err.Error()))
		return fmt.Errorf("decide unread changes: %w", err)
	}
	if err := completion.Result.Validate(); err != nil {
		logger.ErrorContext(ctx, "validate unread change decision", slog.String("err", err.Error()))
		return fmt.Errorf("validate unread change decision: %w", err)
	}
	completion.Result.FileAssessments = fileDecisions
	if reason, err := assessmentPersistenceReason(fileDecisions, service.reviewPolicy); err != nil {
		return err
	} else if reason != "" {
		completion.Result.DecisionReason = reason
	}
	pass.replaceOmissionDecision(completion.Result, service.reviewPolicy)
	logger.InfoContext(
		ctx,
		"unread changes decided from current pull request",
		slog.Bool("acceptable", completion.Result.OmissionsAcceptable),
		slog.Int("unread_hunks", len(shortfall.Hunks)),
		slog.String("model", completion.Model),
	)
	return nil
}

// BuildPullRequestContext renders the configured pull request context.
func BuildPullRequestContext(
	pullRequest githubapp.PullRequest,
	files []diff.FileContext,
	policy reviewrules.Policy,
) (string, error) {
	var contextText strings.Builder
	fmt.Fprintf(
		&contextText,
		"Title: %s\nDescription: %s\nEvidence ID: pull_request\nChanged files:",
		truncateUTF8(escapeOmissionPromptText(pullRequest.Title, policy), maximumPullRequestDescriptionBytes),
		truncateUTF8(escapeOmissionPromptText(pullRequest.Body, policy), maximumPullRequestDescriptionBytes),
	)
	for _, file := range files {
		fmt.Fprintf(
			&contextText,
			"\n- %s (%s)",
			escapeOmissionPromptText(file.Path, policy),
			escapeOmissionPromptText(file.Status, policy),
		)
	}
	contextText.WriteString("\nEvidence ID: changed_files\n")
	var data reviewrules.PromptData
	data.Input = policy.WrapUntrusted(contextText.String())
	return renderPrompt(policy, "pull_request.input", data)
}

func reviewContext(pullRequest githubapp.PullRequest, files []diff.FileContext, shortfall structuralShortfall, maximumBytes int, policy reviewrules.Policy) (string, error) {
	pullRequestContext, err := BuildPullRequestContext(pullRequest, files, policy)
	if err != nil {
		return "", err
	}
	omissions, err := omissionPrompt(shortfall, files, maximumBytes, policy)
	if err != nil {
		return "", err
	}
	return pullRequestContext + omissions, nil
}

func escapeOmissionPromptText(text string, policy reviewrules.Policy) string {
	escaped := runlog.EscapeLineBreaks(text)
	return policy.EscapeUntrusted(escaped)
}

func encodeOmissionMarker(hunks []unreadHunk) string {
	if len(hunks) == 0 {
		return ""
	}
	payload, err := json.Marshal(boundedOmissionMarkerHunks(hunks))
	if err != nil {
		return ""
	}
	return omissionMarkerPrefix + base64.RawURLEncoding.EncodeToString(payload) + " -->"
}

func boundedOmissionMarkerHunks(hunks []unreadHunk) []unreadHunk {
	bounded := sortedUnreadHunks(hunks)
	if len(bounded) > maximumListedUnreadHunks {
		retained := maximumListedUnreadHunks - 1
		omitted := len(bounded) - retained
		bounded = append(bounded[:retained], unreadHunk{
			Path:   "Additional omissions",
			Header: "",
			Reason: fmt.Sprintf("%d more not listed here", omitted),
			Target: nil,
		})
	}
	for index := range bounded {
		bounded[index].Path = truncateUTF8(
			runlog.EscapeLineBreaks(bounded[index].Path), maximumUnreadHunkLabelBytes,
		)
		bounded[index].Header = truncateUTF8(
			runlog.EscapeLineBreaks(bounded[index].Header), maximumUnreadHunkLabelBytes,
		)
	}
	return bounded
}

func decodeOmissionMarker(body string) []unreadHunk {
	_, payload, found := strings.Cut(body, omissionMarkerPrefix)
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
	var hunks []unreadHunk
	if err := json.Unmarshal(decoded, &hunks); err != nil {
		return nil
	}
	return hunks
}

func encodeOmissionDecisionMarker(accepted bool) string {
	return fmt.Sprintf("%s%t -->", omissionDecisionMarkerPrefix, accepted)
}

func decodeOmissionDecisionMarker(body string) (bool, bool) {
	_, payload, found := strings.Cut(body, omissionDecisionMarkerPrefix)
	if !found {
		return false, false
	}
	payload, _, found = strings.Cut(payload, " -->")
	if !found {
		return false, false
	}
	if payload == "true" {
		return true, true
	}
	if payload == "false" {
		return false, true
	}
	return false, false
}

func encodeDecisionReasonMarker(reason string) string {
	reason = sanitizeDecisionReason(reason)
	if reason == "" {
		return ""
	}
	return decisionReasonMarkerPrefix + base64.RawURLEncoding.EncodeToString([]byte(reason)) + " -->"
}

func decodeDecisionReasonMarker(body string) string {
	_, payload, found := strings.Cut(body, decisionReasonMarkerPrefix)
	if !found {
		return ""
	}
	payload, _, found = strings.Cut(payload, " -->")
	if !found {
		return ""
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	return sanitizeDecisionReason(string(decoded))
}

// fileGapReason states why a whole file went unread.
func fileGapReason(gap diff.CoverageGap) string {
	switch gap {
	case diff.CoverageGapBinary:
		return binaryFileReason
	case diff.CoverageGapPatchAbsent:
		return patchAbsentReason
	case diff.CoverageGapPatchUnreadable:
		return patchUnreadableReason
	case diff.CoverageGapContentMissing:
		return contentMissingReason
	case diff.CoverageGapNone, diff.CoverageGapContentUnavailable:
		return ""
	default:
		return ""
	}
}

// concludeStructurallyIncomplete preserves an undecided verdict after provider failure.
// A validated negative omission decision remains blocking.
func (service *Service) concludeStructurallyIncomplete(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
	pullRequest githubapp.PullRequest,
	state marker.State,
	shortfall structuralShortfall,
	summary Summary,
	progress *reviewProgress,
	pass *chunkPass,
) error {
	logger := gklog.L(ctx)
	summary.Decision = domain.ReviewDecisionComment
	summary.Omissions = shortfall.Hunks
	summary.Blocking = replaceBlockingReason(
		summary.Blocking,
		unreviewedHeadReason,
		"The unread changes listed above need review before a verdict.",
	)
	report, reportCalled := service.generateReport(
		ctx, pullRequest, pass,
	)
	summary.Report = report
	summary.Models = pass.analysis().Models
	summary.Usage = UsageFromContext(ctx)
	failures := pass.unreadChunks()
	failureState := pass.scopeFailures(ctx)
	statuses := failureState.statuses
	disposition := domain.AssessmentDeclined
	if failureState.cause != nil {
		disposition = domain.AssessmentIncomplete
	}
	recordAssessment(ctx, disposition, &summary, failureState.classNames(), quotaRecovery(statuses))
	cause := failureState.cause
	addAttemptedModels(&summary, statuses)
	conclusion := checkConclusionDeclined
	if !pass.decidedOmissions() || pass.acceptsOmissions() {
		conclusion = service.presentedConclusion(failureState.classes, conclusion)
	}
	if reportCalled {
		current, err := service.github.GetPullRequest(
			ctx, job.InstallationID, job.Repository, job.Number,
		)
		if err != nil {
			return service.failCheck(
				ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureRefresh, err,
			)
		}
		if current.Head != summary.Head {
			return service.cancelCheck(ctx, job, checkRun.ID)
		}
	}
	dismissErr := service.dismissFailureVerdict(ctx, job, cause)
	var scopeData reviewrules.PromptData
	scopeData.Input = renderUnreadHunks(shortfall.Hunks)
	scopeData.Index = len(shortfall.Hunks)
	scopeData.Total = len(state.Pending)
	notice, err := renderPrompt(service.reviewPolicy, "coverage.pending", scopeData)
	if err != nil {
		return err
	}
	title, err := renderPrompt(service.reviewPolicy, "coverage.check_title", scopeData)
	if err != nil {
		return err
	}
	if reason := chunkFailureReason(failures); reason != "" {
		notice = reason + "\n\n" + notice
	}
	if cause != nil {
		notice += "\n\n" + service.terminalFailureDetail(ctx, job, cause)
	}
	publication := pendingScopePublication{
		job: job, checkRunID: checkRun.ID, state: state, summary: summary,
		notice: notice, title: title, conclusion: conclusion, statuses: statuses,
	}
	if err := service.publishPendingScope(ctx, publication, progress); err != nil {
		return errors.Join(err, dismissErr)
	}
	logger.InfoContext(
		ctx,
		"review head holds changes this service cannot read",
		slog.Int("unread_hunks", len(shortfall.Hunks)),
		slog.Any("unread_paths", shortfall.paths()),
		slog.Int("pending", len(state.Pending)),
		slog.Int64("check_run_id", checkRun.ID),
	)
	if dismissErr != nil {
		logger.WarnContext(ctx, "unread-content review withdrawal failed", slog.String("err", dismissErr.Error()))
		return &reportedFailureError{cause: dismissErr}
	}
	return nil
}

func renderPendingScopeBody(summary Summary, notice string, policy reviewrules.Policy, statuses ...ProviderStatus) (string, error) {
	var data reviewrules.PromptData
	verdict, err := renderPrompt(policy, "coverage.verdict_pending", data)
	if err != nil {
		return "", err
	}
	return renderUnreadableBody(summary, notice, verdict, statuses...), nil
}

// maximumListedUnreadHunks bounds the list a notice prints.
//
// A check run output is capped by size, and one line per unread hunk over a
// pull request touching hundreds of files runs past that cap, which leaves the
// check unfinished and reports nothing at all. The count in the sentence above
// stays exact; only the list is cut.
const maximumListedUnreadHunks = 40

// maximumUnreadHunkLabelBytes bounds one line of that list, because a single
// repository path can be long enough to crowd out the rest on its own.
const maximumUnreadHunkLabelBytes = 220

// renderUnreadHunks lists each unread hunk as inert code text.
func renderUnreadHunks(hunks []unreadHunk) string {
	listed := hunks
	omitted := 0
	if len(listed) > maximumListedUnreadHunks {
		omitted = len(listed) - maximumListedUnreadHunks
		listed = listed[:maximumListedUnreadHunks]
	}
	lines := make([]string, 0, len(listed)+4)
	lines = append(lines, "The model did not read these changes:", "```")
	for _, hunk := range listed {
		lines = append(lines, describeUnreadHunk(hunk))
	}
	lines = append(lines, "```")
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("This list omits %d more unread changes.", omitted))
	}
	return strings.Join(lines, "\n")
}

// describeUnreadHunk names one unread piece the way a reader can go and find it.
func describeUnreadHunk(hunk unreadHunk) string {
	label := escapeUnreadHunkText(hunk.sourceLabel())
	if hunk.Header != "" {
		label += " " + escapeUnreadHunkText(hunk.Header)
	}
	if len(label) > maximumUnreadHunkLabelBytes {
		label = truncateUTF8(label, maximumUnreadHunkLabelBytes) + "..."
	}
	if hunk.Reason == "" {
		return label
	}
	return label + " (" + hunk.Reason + ")"
}

// truncateUTF8 keeps the byte limit without splitting a rune.
func truncateUTF8(text string, maximumBytes int) string {
	end := min(len(text), maximumBytes)
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end]
}

// escapeUnreadHunkText prevents repository text from closing the code fence.
func escapeUnreadHunkText(text string) string {
	withoutLines := runlog.EscapeLineBreaks(text)
	return strings.ReplaceAll(withoutLines, "`", string(rune(0x2CB)))
}

// RenderUnreadableBody renders the visible comment for a head this service
// cannot read whole.
//
// It carries no review marker, for the same reason the progress body carries
// none: that marker means this head was reviewed, and this comment says the
// opposite.
func RenderUnreadableBody(summary Summary, notice string, statuses ...ProviderStatus) string {
	return renderUnreadableBody(summary, notice, "The review submitted no verdict.", statuses...)
}

func renderUnreadableBody(summary Summary, notice, verdict string, statuses ...ProviderStatus) string {
	parts := []string{
		"## Review",
		"### Summary\n\n" + renderReportSummary(summary.Report),
		"### Changes\n\n" + renderWalkthrough(summary.Report),
		"### Omissions\n\n" + notice,
		verdictSectionStart,
		"### Verdict",
		verdict,
	}
	if findings := renderSummaryFindings(summary); findings != "" {
		parts = append(parts, findings)
	}
	parts = append(parts, verdictSectionEnd, RenderDetails(summary, statuses...))
	if checkpoints := renderFindingCheckpoints(summary); checkpoints != "" {
		parts = append(parts, checkpoints)
	}
	return strings.Join(parts, "\n\n")
}

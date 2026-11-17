package review

import (
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// shortHeadLength is how much of a head SHA the review details show.
const shortHeadLength = 7

// Summary is everything one published review reports about itself. The visible
// comment and the check run both render from this one value, so the two can
// never disagree.
type Summary struct {
	Outcome                    domain.AssessmentOutcome
	Result                     string
	FileAssessmentNote         string
	FileAssessmentMaximumBytes int
	FileAssessments            []domain.FileAssessment
	FileAssessmentsDisplay     reviewrules.FindingDisplay
	Head                       domain.HeadSHA
	Decision                   domain.ReviewDecision
	DecisionReason             string
	ApprovalWithheld           bool
	Report                     Report
	OmissionsAccepted          bool
	// Blocking states what a requesting-changes verdict is waiting on, one
	// entry per cause. A block that names nothing reads as a silent repeat, so
	// a blocking verdict always carries at least one entry here.
	Blocking                []string
	Models                  []string
	Usage                   UsageSummary
	Duration                time.Duration
	FilesReviewed           int
	Chunks                  int
	CoverageComplete        bool
	MinimumImportance       int
	Observed                []domain.Finding
	Eligible                []domain.Finding
	Published               []domain.Finding
	Fallback                []domain.Finding
	Metadata                []domain.Finding
	metadataCheckpoint      *findingCheckpoint
	fallbackCheckpoint      *findingCheckpoint
	MetadataRevision        string
	MetadataFindingsDisplay reviewrules.FindingDisplay
	Omissions               []unreadHunk
	PriorReviews            []reviewTrace
	Threads                 []threadTrace
	// Reached names the last stage the review completed. A failed review fills
	// only the fields it got far enough to learn, so the reader can tell how far
	// it got rather than seeing zeros with no explanation.
	Reached string
	// Failed marks a review that stopped early, so the detail table reports
	// progress rather than a result.
	Failed     bool
	InProgress bool
	// Forced marks a run a label asked for, so the top-level review comment can
	// explain why the same pull request was reviewed again.
	Forced bool
}

// Verdict states the outcome without presenting withheld approval as rejection.
func (summary Summary) Verdict() (string, error) {
	var rendering renderState

	return rendering.finish(
		rendering.capture(
			renderOutputTemplate(
				"output.verdict_lead",
				reflect.ValueOf(
					struct {
						Decision domain.ReviewDecision
						Metadata bool
					}{
						Decision: summary.Decision,
						Metadata: len(unpublishedSummaryFindings(summary, summary.Metadata)) > 0,
					},
				),
			),
		),
	)
}

// Title names the outcome for the check run.
func (summary Summary) Title() (string, error) {
	var rendering renderState

	return rendering.finish(
		rendering.capture(
			renderOutputTemplate("output.verdict_title", reflect.ValueOf(summary.Decision)),
		),
	)
}

// RenderDetails renders the latest review's raw usage table.
func RenderDetails(summary Summary, statuses ...ProviderStatus) (string, error) {
	var rendering renderState
	if len(summary.Usage.ProviderAttempts) == 0 {
		for _, status := range statuses {
			var attempt ProviderAttempt
			attempt.Status = status
			summary.Usage.ProviderAttempts = append(summary.Usage.ProviderAttempts, attempt)
		}
	}

	return rendering.finish(rendering.capture(renderRawDetails(summary)))
}

func acceptedFileScope(summary Summary) bool {
	if !summary.OmissionsAccepted || summary.Failed {
		return false
	}

	excluded := false

	for _, assessment := range summary.FileAssessments {
		switch assessment.Action {
		case domain.FileActionExclude:
			if !assessment.PermitsContentExclusion() {
				return false
			}

			excluded = true
		case domain.FileActionReview:
			if !assessment.Reviewed {
				return false
			}
		case domain.FileActionInspectRelated, domain.FileActionGap:
			return false
		default:
			return false
		}
	}

	return excluded
}

// RenderBody renders the single visible GitHub review summary.
func RenderBody(summary Summary) (string, error) {
	var rendering renderState
	parts := []string{
		rendering.capture(renderVerdictSection(summary, false)),
		rendering.capture(renderReportSection(summary.Report)),
	}
	if summary.FileAssessmentsDisplay == reviewrules.FindingsExpanded {
		parts = append(
			parts,
			rendering.capture(
				renderFileAssessments(summary.FileAssessments, summary.FileAssessmentMaximumBytes),
			),
		)
	}

	if len(summary.Omissions) > 0 && !summary.OmissionsAccepted {
		parts = append(
			parts,
			rendering.capture(
				renderOutputTemplate(
					"output.omissions",
					reflect.ValueOf(rendering.capture(renderOmissions(summary.Omissions))),
				),
			),
		)
	}

	parts = append(
		parts,
		rendering.capture(RenderDetails(summary)),
		rendering.capture(
			renderOutputTemplate(
				"output.summary_checkpoint",
				reflect.ValueOf(RenderVerdictBody(summary)),
			),
		),
	)

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.review_body", reflect.ValueOf(parts))),
	)
}

func renderReportSection(report Report) (string, error) {
	var rendering renderState

	return rendering.finish(
		rendering.capture(
			renderOutputTemplate(
				"output.report_section",
				reflect.ValueOf(rendering.capture(renderReportSummary(report))),
			),
		),
	)
}

func renderReportSummary(report Report) (string, error) {
	var rendering renderState
	value := sanitizeReportText(report.Summary)
	if value != "" {
		return rendering.finish(value)
	}

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.report_fallback", reflect.ValueOf(nil))),
	)
}

func renderOmissions(hunks []unreadHunk) (string, error) {
	var rendering renderState
	if len(hunks) == 0 {
		return rendering.finish(
			rendering.capture(renderOutputTemplate("output.omissions_empty", reflect.ValueOf(nil))),
		)
	}

	return rendering.finish(rendering.capture(renderUnreadHunks(hunks)))
}

const (
	verdictSectionStart = "<!-- pr-review-agent:verdict-section:start -->"
	verdictSectionEnd   = "<!-- pr-review-agent:verdict-section:end -->"
	reportSectionStart  = "<!-- pr-review-agent:report-section:start -->"
	reportSectionEnd    = "<!-- pr-review-agent:report-section:end -->"
)

func renderVerdictSection(summary Summary, blockWithdrawn bool) (string, error) {
	var rendering renderState
	parts := []string{rendering.capture(verdictLead(summary))}
	if findings := rendering.capture(renderSummaryFindings(summary)); findings != "" {
		parts = append(parts, findings)
	}

	if blockWithdrawn && summary.Decision == domain.ReviewDecisionRequestChanges {
		parts = append(parts, rendering.capture(withdrawnBlockNote()))
	}

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.verdict_section", reflect.ValueOf(parts))),
	)
}

func sanitizeReportText(value string) string {
	value = strings.TrimSpace(sanitizeProse(value))
	value = strings.ReplaceAll(value, "<!--", "&lt;!--")

	return strings.ReplaceAll(value, "-->", "--&gt;")
}

func verdictLead(summary Summary) (string, error) {
	return summary.Verdict()
}

func renderFallbackFindings(findings []domain.Finding) (string, error) {
	var rendering renderState
	if len(findings) == 0 {
		return rendering.finish("")
	}

	sorted := append([]domain.Finding{}, findings...)
	sortFindings(sorted)
	type fallbackDisplay struct {
		Path       string
		Line       int
		Title      string
		Body       string
		Suggestion string
	}

	sections := make([]fallbackDisplay, 0, len(sorted))
	for _, finding := range sorted {
		normalizedPath, err := marker.NormalizePath(finding.Path)
		if err != nil {
			continue
		}

		finding = sanitizeFinding(finding)
		sections = append(
			sections,
			fallbackDisplay{
				Path:       rendering.capture(codeSpan(normalizedPath)),
				Line:       finding.EndLine,
				Title:      finding.Title,
				Body:       finding.Body,
				Suggestion: finding.Suggestion,
			},
		)
	}

	return rendering.finish(
		rendering.capture(
			renderOutputTemplate(
				"output.fallback_findings",
				reflect.ValueOf(
					struct {
						Single   bool
						Findings []fallbackDisplay
					}{
						Single:   len(sorted) == 1,
						Findings: sections,
					},
				),
			),
		),
	)
}

// RenderVerdictBody serializes assessment checkpoints for the editable summary
// and for native reviews that require a body. Native approvals omit this data.
func RenderVerdictBody(summary Summary) string {
	parts := make([]string, 0, 5)

	parts = append(parts, marker.Review(summary.Head, summary.Decision))
	if assessments := encodeFileAssessments(
		summary.FileAssessments,
		summary.FileAssessmentMaximumBytes,
	); assessments != "" {
		parts = append(parts, assessments)
	}

	if summary.ApprovalWithheld || summary.Decision == domain.ReviewDecisionComment {
		parts = append(parts, approvalWithheldMarker)
	}

	if omissions := encodeOmissionMarker(summary.Omissions); omissions != "" {
		parts = append(parts, omissions)
		parts = append(parts, encodeOmissionDecisionMarker(summary.OmissionsAccepted))
	}

	if reason := encodeDecisionReasonMarker(
		scopeDecisionReason(summary.DecisionReason, summary.Omissions, summary.FileAssessments),
	); reason != "" {
		parts = append(parts, reason)
	}

	if checkpoints := renderFindingCheckpoints(summary); checkpoints != "" {
		parts = append(parts, checkpoints)
	}

	return strings.Join(parts, "\n")
}

// A completed read may still have refused omissions or unplaced findings.
// Thread refreshes must preserve that distinction until another review runs.
const approvalWithheldMarker = "<!-- pr-review-agent:approval-withheld -->"

const (
	fallbackSectionStart = "<!-- pr-review-agent:fallback:start -->"
	fallbackSectionEnd   = "<!-- pr-review-agent:fallback:end -->"
)

// withdrawnBlockNote explains a head whose findings are open while no blocking
// verdict stands over them.
//
// Without it the comment lists what the review is waiting on directly above a
// pull request that reads as unblocked, and a reader has no way to tell whether
// the service failed to block or somebody cleared the block by hand. Naming the
// dismissal is also what tells them how to get a verdict back.
func withdrawnBlockNote() (string, error) {
	var rendering renderState

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.withdrawn_block", reflect.ValueOf(nil))),
	)
}

// renderVerdictRefreshProse is the summary comment prose for a verdict
// refreshed from thread state alone. It reports no run statistics because no
// model ran; the verdict and what it still waits on are the whole story.
//
// blockWithdrawn says a person dismissed the verdict at this head, which the
// prose has to carry because the refresh then submits no blocking review and
// the comment is the only place a reader learns why.
func renderVerdictRefreshProse(
	summary Summary,
	blockWithdrawn bool,
	details string,
	reportPrefix string,
) (string, error) {
	var rendering renderState
	parts := []string{
		rendering.capture(
			renderVerdictBeforeReport(
				reportPrefix,
				rendering.capture(renderVerdictSection(summary, blockWithdrawn)),
			),
		),
	}
	if len(summary.Omissions) > 0 && !summary.OmissionsAccepted {
		parts = append(
			parts,
			rendering.capture(
				renderOutputTemplate(
					"output.omissions",
					reflect.ValueOf(rendering.capture(renderOmissions(summary.Omissions))),
				),
			),
		)
	}

	parts = append(
		parts,
		details,
		rendering.capture(
			renderOutputTemplate(
				"output.summary_checkpoint",
				reflect.ValueOf(RenderVerdictBody(summary)),
			),
		),
	)

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.paragraphs", reflect.ValueOf(parts))),
	)
}

func refreshVerdictProse(
	existingBody string,
	summary Summary,
	blockWithdrawn bool,
) (string, error) {
	var rendering renderState
	if len(summary.FileAssessments) == 0 {
		summary.FileAssessments = decodeFileAssessments(existingBody)
	}

	prose := stripMaintenanceFailure(stripDurableStateMarker(existingBody))

	fallbackRecord, recordedFallback := readFallbackRecord(prose, summary.Head)
	if summary.ApprovalWithheld && len(summary.Fallback) == 0 && recordedFallback {
		summary.Fallback = fallbackRecord.Findings
	}

	start := strings.Index(prose, verdictSectionStart)
	if start < 0 {
		return rendering.finish(
			rendering.capture(
				renderVerdictRefreshProse(
					summary,
					blockWithdrawn,
					rendering.capture(preservedReviewDetails(prose, summary)),
					rendering.capture(preservedReportPrefix(prose)),
				),
			),
		)
	}

	endOffset := strings.Index(prose[start:], verdictSectionEnd)
	if endOffset < 0 {
		return rendering.finish(
			rendering.capture(
				renderVerdictRefreshProse(
					summary,
					blockWithdrawn,
					rendering.capture(preservedReviewDetails(prose, summary)),
					rendering.capture(preservedReportPrefix(prose)),
				),
			),
		)
	}

	end := start + endOffset + len(verdictSectionEnd)

	verdict := rendering.capture(renderVerdictSection(summary, blockWithdrawn))
	if summary.ApprovalWithheld && len(summary.Fallback) == 0 && !recordedFallback {
		if fallbackStart := strings.Index(
			prose[start:end],
			fallbackSectionStart,
		); fallbackStart >= 0 {
			fallback := prose[start+fallbackStart : end]
			if fallbackEnd := strings.Index(fallback, fallbackSectionEnd); fallbackEnd >= 0 {
				retained := fallback[:fallbackEnd+len(fallbackSectionEnd)]
				verdict = strings.Replace(
					verdict,
					verdictSectionEnd,
					rendering.capture(renderOutputParts(retained, verdictSectionEnd)),
					1,
				)
			}
		}
	}

	updated := rendering.capture(
		renderVerdictBeforeReport(strings.TrimSpace(prose[:start]), verdict),
	) + prose[end:]

	updated = rendering.capture(refreshThreadDetails(updated, summary))
	if markerStart := strings.LastIndex(updated, marker.Summary()); markerStart >= 0 {
		updated = rendering.capture(
			renderOutputParts(
				strings.TrimSpace(updated[:markerStart]),
				rendering.capture(
					renderOutputTemplate(
						"output.summary_checkpoint",
						reflect.ValueOf(RenderVerdictBody(summary)),
					),
				),
			),
		)
	}

	return rendering.finish(updated)
}

func renderVerdictBeforeReport(reportPrefix, verdict string) (string, error) {
	var rendering renderState
	heading, report, _ := strings.Cut(reportPrefix, "\n\n")
	parts := []string{heading, verdict}

	if report != "" {
		if !strings.HasPrefix(report, reportSectionStart) {
			report = rendering.capture(
				renderOutputTemplate("output.report_wrapper", reflect.ValueOf(report)),
			)
		}

		parts = append(parts, report)
	}

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.paragraphs", reflect.ValueOf(parts))),
	)
}

func stripDurableStateMarker(body string) string {
	const stateMarker = "\n<!-- pr-review-agent:state:v1 "
	if index := strings.LastIndex(body, stateMarker); index >= 0 {
		return strings.TrimSpace(body[:index])
	}

	return strings.TrimSpace(body)
}

func refreshThreadDetails(body string, summary Summary) (string, error) {
	var rendering renderState
	rows := map[string]string{
		"Bot thread IDs": rendering.capture(
			renderOutputTemplate(
				"output.thread_ids_row",
				reflect.ValueOf(rendering.capture(formatThreadTraceIDs(summary.Threads))),
			),
		),
		"Bot threads open": rendering.capture(
			renderOutputTemplate(
				"output.threads_open_row",
				reflect.ValueOf(countOpenThreadTraces(summary.Threads)),
			),
		),
		"Bot threads resolved": rendering.capture(
			renderOutputTemplate(
				"output.threads_resolved_row",
				reflect.ValueOf(countResolvedThreadTraces(summary.Threads)),
			),
		),
	}

	lines := strings.Split(body, "\n")
	for index, line := range lines {
		for label, row := range rows {
			if strings.HasPrefix(line, "| "+label+" |") {
				lines[index] = row
			}
		}
	}

	return rendering.finish(strings.Join(lines, "\n"))
}

// renderBlocking links the inline findings that a reader must resolve.
func renderBlocking(reasons []string) (string, error) {
	var rendering renderState
	if len(reasons) == 0 {
		return rendering.finish("")
	}

	resolved := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		resolved = append(resolved, rendering.capture(renderOutputText(reason)))
	}

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.blocking", reflect.ValueOf(resolved))),
	)
}

func formatYesNo(value bool) (string, error) {
	var rendering renderState

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.boolean", reflect.ValueOf(value))),
	)
}

func shortHead(head domain.HeadSHA) string {
	value := string(head)
	if len(value) <= shortHeadLength {
		return value
	}

	return value[:shortHeadLength]
}

// RenderStartedBody renders the comment a run posts before it reads anything.
//
// It exists because the pull request said nothing at all until the first chunk
// came back, which on a large delta is minutes of silence with a pending check
// and no way to tell a slow review from one that never began. The comment is
// posted as soon as the run knows it is reviewing this head, and every later
// stage rewrites the same comment rather than adding another.
//
// The comment does not include a review marker because the next attempt would
// otherwise treat this unreviewed head as already reviewed.
func RenderStartedBody(head domain.HeadSHA) (string, error) {
	var rendering renderState

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.started", reflect.ValueOf(shortHead(head)))),
	)
}

// RenderFailureBody renders the visible summary for a review that could not
// finish. It carries the same detail table as a successful review, reporting
// how far the review got, and it omits the review marker so the next attempt on
// the same head is not suppressed as already reviewed.
//
// title and detail contain service-written text rather than provider messages.
// The renderer can include these trusted strings without fencing or escaping.
func RenderFailureBody(summary Summary, title string, detail string) (string, error) {
	var rendering renderState
	summary.Failed = true

	parts := []string{strings.TrimSpace(rendering.capture(renderOutputText(title)))}
	if trimmedDetail := strings.TrimSpace(detail); trimmedDetail != "" {
		parts = append(parts, trimmedDetail)
	}

	hasVerdict := summary.ApprovalWithheld && summary.Decision != ""
	if hasVerdict {
		summary.Decision = domain.ReviewDecisionComment
		parts = append(
			parts,
			rendering.capture(renderVerdictSection(summary, false)),
			RenderVerdictBody(summary),
		)
	} else if findings := rendering.capture(renderSummaryFindings(summary)); findings != "" {
		parts = append(parts, findings)
	}

	if !hasVerdict {
		if checkpoints := renderFindingCheckpoints(summary); checkpoints != "" {
			parts = append(parts, checkpoints)
		}
	}

	parts = append(parts, rendering.capture(RenderDetails(summary)), marker.Summary())

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.review_body", reflect.ValueOf(parts))),
	)
}

// RenderSkipBody renders the notice when admission declines a delta.
// The notice omits the review marker because the admission gate declines the
// delta before any model call and does not operate on review objects.
func RenderSkipBody(reason string) (string, error) {
	var rendering renderState

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.skipped", reflect.ValueOf(reason))),
	)
}

// RenderProgressBody omits the review marker when remaining is zero.
func RenderProgressBody(head domain.HeadSHA, remaining int) (string, error) {
	var rendering renderState

	return rendering.finish(
		rendering.capture(
			renderOutputTemplate(
				"output.progress",
				reflect.ValueOf(
					struct {
						Head      string
						Remaining int
						Chunks    string
					}{
						Head:      shortHead(head),
						Remaining: remaining,
						Chunks:    rendering.capture(chunkCount(remaining)),
					},
				),
			),
		),
	)
}

// codeSpan renders repository-controlled text as an inline code span that the
// text cannot break out of.
//
// A path is whatever the pull request named a file, so it reaches this comment
// under the service's own identity. A backtick would close the span and let the
// rest render as Markdown, and a line break would end the list item and let the
// remainder pose as the service's own prose, which is how a crafted filename
// puts a mention or a false verdict in a comment nobody would doubt.
func codeSpan(text string) (string, error) {
	var rendering renderState
	flattened := strings.ReplaceAll(replyLineBreaks.Replace(text), "\n", " ")

	return rendering.finish(
		rendering.capture(
			renderOutputTemplate(
				"output.code_span",
				reflect.ValueOf(
					struct {
						Delimiter string
						Value     string
					}{
						Delimiter: "`",
						Value:     strings.ReplaceAll(flattened, "`", string(rune(0x2CB))),
					},
				),
			),
		),
	)
}

// RenderIncompleteBody renders the visible comment for a pass that could not
// read every chunk it owed.
//
// It states what is left and how to retry this head, and it carries no
// review marker for the same reason the progress body carries none. reason is
// this service's own wording for what went wrong; the provider's own sentence
// never reaches here, because a pull request comment is public and permanent.
//
// The table reports coverage rather than a stage. This run reached the end and
// published a verdict, so there is no stage it stopped at, and a chunk nobody
// read is exactly what the coverage row is for.
func RenderIncompleteBody(
	summary Summary,
	pending int,
	reason string,
	detail string,
	statuses ...ProviderStatus,
) (string, error) {
	var rendering renderState
	legacyDaily := false
	budgetFailure := reason == checkFailureDailyBudget || reason == checkFailureMixedUsage
	if budgetFailure {
		for _, status := range statuses {
			if status.Cause == ProviderAppBudgetDenied && status.TokenWindow.Mode == "" {
				legacyDaily = true
				break
			}
		}
	}

	lead := rendering.capture(
		renderOutputTemplate(
			"output.incomplete_lead",
			reflect.ValueOf(
				struct {
					Chunks      string
					Head        string
					LegacyDaily bool
					Retry       bool
					Label       string
				}{
					Chunks:      rendering.capture(chunkCount(pending)),
					Head:        shortHead(summary.Head),
					LegacyDaily: legacyDaily,
					Retry:       !budgetFailure,
					Label:       domain.RerunReviewLabel,
				},
			),
		),
	)
	blocking := removeBlockingReasons(summary.Blocking, unreviewedHeadReason)
	parts := []string{lead}

	for _, note := range []string{
		reason,
		unreviewedHeadReason,
		rendering.capture(renderBlocking(blocking)),
		detail,
	} {
		if trimmed := strings.TrimSpace(rendering.capture(renderOutputText(note))); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}

	if findings := rendering.capture(renderSummaryFindings(summary)); findings != "" {
		parts = append(parts, findings)
	}

	if checkpoints := renderFindingCheckpoints(summary); checkpoints != "" {
		parts = append(parts, checkpoints)
	}

	parts = append(parts, rendering.capture(RenderDetails(summary, statuses...)))

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.review_body", reflect.ValueOf(parts))),
	)
}

func removeBlockingReasons(reasons []string, removed ...string) []string {
	kept := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if !slices.Contains(removed, reason) {
			kept = append(kept, reason)
		}
	}

	return kept
}

func replaceBlockingReason(reasons []string, oldReason string, newReason string) []string {
	replaced := append([]string{}, reasons...)
	for index, reason := range replaced {
		if reason == oldReason {
			replaced[index] = newReason
		}
	}

	return replaced
}

// chunkCount names a number of chunks without the plural mismatch a bare
// count leaves in a sentence a person reads.
func chunkCount(count int) (string, error) {
	var rendering renderState

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.chunk_count", reflect.ValueOf(count))),
	)
}

// RenderInline renders anchored findings as GitHub inline review comments.
func RenderInline(
	head domain.HeadSHA,
	findings []domain.Finding,
) ([]githubapp.InlineComment, error) {
	sorted := append([]domain.Finding{}, findings...)
	sortFindings(sorted)

	comments := make([]githubapp.InlineComment, 0, len(sorted))
	for _, finding := range sorted {
		normalizedPath, err := marker.NormalizePath(finding.Path)
		if err != nil {
			slog.Error("normalize finding path", slog.String("err", err.Error()))
			return nil, fmt.Errorf("normalize finding path: %w", err)
		}

		// The evidence and the claim sentence reach the encoder, and no further.
		// Each is hashed into one of the marker's keys, which is what lets a
		// later run recognize a reworded restatement of this same claim; the
		// rendered body still prints neither, so the reader sees no quoted
		// source line they already have beside the comment, and no label the
		// model wrote for the service rather than for them.
		body, err := marker.EncodeFindingBody(
			head,
			domain.Finding{
				Surface:          domain.FindingFile,
				CommitSHA:        "",
				CorrectionAction: finding.CorrectionAction,
				RuleID:           finding.RuleID,
				Deduplication:    finding.Deduplication,
				Path:             normalizedPath,
				StartLine:        finding.StartLine,
				EndLine:          finding.EndLine,
				Title:            sanitizeProse(finding.Title),
				Body:             sanitizeProse(finding.Body),
				Evidence:         finding.Evidence,
				Claim:            finding.Claim,
				Suggestion:       finding.Suggestion,
				Importance:       finding.Importance,
			},
		)
		if err != nil {
			slog.Error("encode finding body", slog.String("err", err.Error()))
			return nil, fmt.Errorf("encode finding body: %w", err)
		}

		comment := githubapp.InlineComment{
			Path:      normalizedPath,
			Body:      body,
			Line:      finding.EndLine,
			Side:      "RIGHT",
			StartLine: 0,
			StartSide: "",
		}
		if finding.StartLine != finding.EndLine {
			comment.StartLine = finding.StartLine
			comment.StartSide = "RIGHT"
		}

		comments = append(comments, comment)
	}

	return comments, nil
}

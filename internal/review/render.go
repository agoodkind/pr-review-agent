package review

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// shortHeadLength is how much of a head SHA the review details show.
const shortHeadLength = 7

// Summary is everything one published review reports about itself. The visible
// comment and the check run both render from this one value, so the two can
// never disagree.
type Summary struct {
	Head              domain.HeadSHA
	Decision          domain.ReviewDecision
	DecisionReason    string
	ApprovalWithheld  bool
	Report            Report
	OmissionsAccepted bool
	// Blocking states what a requesting-changes verdict is waiting on, one
	// entry per cause. A block that names nothing reads as a silent repeat, so
	// a blocking verdict always carries at least one entry here.
	Blocking          []string
	Models            []string
	Usage             UsageSummary
	Duration          time.Duration
	FilesReviewed     int
	Chunks            int
	CoverageComplete  bool
	MinimumImportance int
	Observed          []domain.Finding
	Eligible          []domain.Finding
	Published         []domain.Finding
	Fallback          []domain.Finding
	Omissions         []unreadHunk
	PriorReviews      []reviewTrace
	Threads           []threadTrace
	// Reached names the last stage the review completed. A failed review fills
	// only the fields it got far enough to learn, so the reader can tell how far
	// it got rather than seeing zeros with no explanation.
	Reached string
	// Failed marks a review that stopped early, so the detail table reports
	// progress rather than a result.
	Failed bool
	// Forced marks a run a label asked for, so the top-level review comment can
	// explain why the same pull request was reviewed again.
	Forced bool
}

// Verdict states the outcome without presenting withheld approval as rejection.
func (summary Summary) Verdict() string {
	if summary.Decision == domain.ReviewDecisionComment {
		return "This review neither approves nor requests changes."
	}
	if summary.Decision != domain.ReviewDecisionRequestChanges {
		return "This review found no severe defects."
	}
	return "Resolve the open inline findings."
}

// Title names the outcome for the check run.
func (summary Summary) Title() string {
	if summary.Decision == domain.ReviewDecisionComment {
		return "Review needs attention"
	}
	if summary.Decision == domain.ReviewDecisionRequestChanges {
		return "Changes requested"
	}
	return "Approved"
}

// RenderDetails renders the collapsed review detail table. A failed review
// reports how far it got, so the same table explains both outcomes.
func RenderDetails(summary Summary) string {
	rows := [][2]string{
		{"Model", formatModels(summary.Models)},
		{"Duration", formatDuration(summary.Duration)},
		{"Head", "`" + shortHead(summary.Head) + "`"},
		{"Files reviewed", fmt.Sprintf("`%d`", summary.FilesReviewed)},
		{"Diff chunks", fmt.Sprintf("`%d`", summary.Chunks)},
	}
	if summary.Failed {
		rows = append(rows, [2]string{"Reached", formatReached(summary.Reached)})
	} else {
		rows = append(rows, [2]string{"Coverage complete", formatYesNo(summary.CoverageComplete)})
	}
	rows = append(rows,
		[2]string{"Forced run", formatYesNo(summary.Forced)},
		[2]string{"Minimum importance", fmt.Sprintf("`%d`", summary.MinimumImportance)},
		[2]string{"Findings observed", formatCountAndImportances(summary.Observed)},
		[2]string{"Findings eligible", formatCountAndImportances(summary.Eligible)},
		[2]string{"Findings published inline", formatCountAndImportances(summary.Published)},
		[2]string{"Prior bot review IDs", formatReviewTraceIDs(summary.PriorReviews)},
		[2]string{"Bot thread IDs", formatThreadTraceIDs(summary.Threads)},
		[2]string{"Bot threads open", fmt.Sprintf("`%d`", countOpenThreadTraces(summary.Threads))},
		[2]string{"Bot threads resolved", fmt.Sprintf("`%d`", countResolvedThreadTraces(summary.Threads))},
	)

	var builder strings.Builder
	builder.WriteString("<details>\n<summary>Review details</summary>\n\n")
	if reason := sanitizeDecisionReason(summary.DecisionReason); reason != "" {
		builder.WriteString("#### Omission decision\n\n" + reason + "\n\n")
	}
	builder.WriteString("| | |\n| --- | --- |\n")
	for _, row := range rows {
		fmt.Fprintf(&builder, "| %s | %s |\n", row[0], row[1])
	}
	builder.WriteString(renderUsageDetails(summary.Usage))
	builder.WriteString("\n</details>")
	return builder.String()
}

// RenderBody renders the single visible GitHub review summary.
func RenderBody(summary Summary) string {
	parts := []string{
		"## Review",
		"### Summary\n\n" + renderReportSummary(summary.Report),
		"### Changes\n\n" + renderWalkthrough(summary.Report),
		renderVerdictSection(summary, false),
	}
	if len(summary.Omissions) > 0 {
		parts = append(parts, "### Omissions\n\n"+renderOmissions(summary.Omissions))
	}
	parts = append(parts, RenderDetails(summary), marker.Summary()+"\n"+RenderVerdictBody(summary))
	return strings.Join(parts, "\n\n")
}

func renderReportSummary(report Report) string {
	report = SanitizeReport(report)
	value := sanitizeReportText(report.Summary)
	if value != "" {
		return value
	}
	return "This review checked the current pull request against its stated purpose and current changes."
}

func renderWalkthrough(report Report) string {
	report = SanitizeReport(report)
	lines := make([]string, 0, len(report.Walkthrough))
	for _, item := range report.Walkthrough {
		if value := sanitizeReportText(item); value != "" {
			lines = append(lines, "- "+value)
		}
	}
	if len(lines) == 0 {
		return "The review examined the changed files and their current diff."
	}
	return strings.Join(lines, "\n")
}

func renderOmissions(hunks []unreadHunk) string {
	if len(hunks) == 0 {
		return "The review omitted no changed content."
	}
	return renderUnreadHunks(hunks)
}

const (
	verdictSectionStart = "<!-- pr-review-agent:verdict-section:start -->"
	verdictSectionEnd   = "<!-- pr-review-agent:verdict-section:end -->"
)

func renderVerdictSection(summary Summary, blockWithdrawn bool) string {
	parts := []string{verdictSectionStart, "### Verdict", verdictLead(summary)}
	if fallback := renderFallbackFindings(summary.Fallback); fallback != "" {
		parts = append(parts, fallbackSectionStart+"\n"+fallback+"\n"+fallbackSectionEnd)
	}
	if blockWithdrawn && summary.Decision == domain.ReviewDecisionRequestChanges {
		parts = append(parts, withdrawnBlockNote)
	}
	parts = append(parts, verdictSectionEnd)
	return strings.Join(parts, "\n\n")
}

func sanitizeReportText(value string) string {
	value = strings.TrimSpace(sanitizeProse(value))
	value = strings.ReplaceAll(value, "<!--", "&lt;!--")
	return strings.ReplaceAll(value, "-->", "--&gt;")
}

func verdictLead(summary Summary) string {
	return summary.Verdict()
}

func renderFallbackFindings(findings []domain.Finding) string {
	if len(findings) == 0 {
		return ""
	}
	sorted := append([]domain.Finding{}, findings...)
	sortFindings(sorted)
	explanation := "GitHub could not place the finding inline, so it appears here."
	if len(sorted) != 1 {
		explanation = "GitHub could not place the findings inline, so they appear here."
	}
	sections := []string{explanation, "#### Findings"}
	for _, finding := range sorted {
		normalizedPath, err := marker.NormalizePath(finding.Path)
		if err != nil {
			continue
		}
		finding = sanitizeFinding(finding)
		parts := []string{
			fmt.Sprintf("#### %s:%d: %s", codeSpan(normalizedPath), finding.EndLine, finding.Title),
			finding.Body,
		}
		if finding.Suggestion != "" {
			parts = append(parts, "```suggestion\n"+finding.Suggestion+"\n```")
		}
		sections = append(sections, strings.Join(parts, "\n\n"))
	}
	return strings.Join(sections, "\n\n")
}

// RenderVerdictBody renders the review object that carries the verdict.
//
// GitHub displays the decision itself. The hidden markers preserve review state
// without creating a third visible comment beside the top-level report and the
// inline findings.
func RenderVerdictBody(summary Summary) string {
	parts := make([]string, 0, 5)
	parts = append(parts, marker.Review(summary.Head, summary.Decision))
	if summary.ApprovalWithheld || summary.Decision == domain.ReviewDecisionComment {
		parts = append(parts, approvalWithheldMarker)
	}
	if omissions := encodeOmissionMarker(summary.Omissions); omissions != "" {
		parts = append(parts, omissions)
		parts = append(parts, encodeOmissionDecisionMarker(summary.OmissionsAccepted))
	}
	if reason := encodeDecisionReasonMarker(summary.DecisionReason); reason != "" {
		parts = append(parts, reason)
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
const withdrawnBlockNote = "The blocking review on this commit was dismissed by hand, so this service " +
	"is not reinstating it. The findings above are still open, and resolving them lets the next " +
	"refresh approve."

// renderVerdictRefreshProse is the summary comment prose for a verdict
// refreshed from thread state alone. It reports no run statistics because no
// model ran; the verdict and what it still waits on are the whole story.
//
// blockWithdrawn says a person dismissed the verdict at this head, which the
// prose has to carry because the refresh then submits no blocking review and
// the comment is the only place a reader learns why.
func renderVerdictRefreshProse(summary Summary, blockWithdrawn bool) string {
	parts := []string{"## Review", renderVerdictSection(summary, blockWithdrawn)}
	if len(summary.Omissions) > 0 {
		parts = append(parts, "### Omissions\n\n"+renderOmissions(summary.Omissions))
	}
	parts = append(
		parts,
		RenderDetails(summary),
		marker.Summary()+"\n"+marker.Review(summary.Head, summary.Decision),
	)
	return strings.Join(parts, "\n\n")
}

func refreshVerdictProse(existingBody string, summary Summary, blockWithdrawn bool) string {
	prose := stripDurableStateMarker(existingBody)
	start := strings.Index(prose, verdictSectionStart)
	if start < 0 {
		return renderVerdictRefreshProse(summary, blockWithdrawn)
	}
	endOffset := strings.Index(prose[start:], verdictSectionEnd)
	if endOffset < 0 {
		return renderVerdictRefreshProse(summary, blockWithdrawn)
	}
	end := start + endOffset + len(verdictSectionEnd)
	verdict := renderVerdictSection(summary, blockWithdrawn)
	if summary.ApprovalWithheld && len(summary.Fallback) == 0 {
		if fallbackStart := strings.Index(prose[start:end], fallbackSectionStart); fallbackStart >= 0 {
			fallback := prose[start+fallbackStart : end]
			if fallbackEnd := strings.Index(fallback, fallbackSectionEnd); fallbackEnd >= 0 {
				retained := fallback[:fallbackEnd+len(fallbackSectionEnd)]
				verdict = strings.Replace(verdict, verdictSectionEnd, retained+"\n\n"+verdictSectionEnd, 1)
			}
		}
	}
	updated := strings.TrimSpace(prose[:start]) + "\n\n" +
		verdict + prose[end:]
	updated = refreshThreadDetails(updated, summary)
	if markerStart := strings.LastIndex(updated, marker.Summary()); markerStart >= 0 {
		updated = strings.TrimSpace(updated[:markerStart]) + "\n\n" + marker.Summary() + "\n" +
			RenderVerdictBody(summary)
	}
	return updated
}

func stripDurableStateMarker(body string) string {
	const stateMarker = "\n<!-- pr-review-agent:state:v1 "
	if index := strings.LastIndex(body, stateMarker); index >= 0 {
		return strings.TrimSpace(body[:index])
	}
	return strings.TrimSpace(body)
}

func refreshThreadDetails(body string, summary Summary) string {
	rows := map[string]string{
		"Bot thread IDs":       formatThreadTraceIDs(summary.Threads),
		"Bot threads open":     fmt.Sprintf("`%d`", countOpenThreadTraces(summary.Threads)),
		"Bot threads resolved": fmt.Sprintf("`%d`", countResolvedThreadTraces(summary.Threads)),
	}
	lines := strings.Split(body, "\n")
	for index, line := range lines {
		for label, value := range rows {
			if strings.HasPrefix(line, "| "+label+" |") {
				lines[index] = "| " + label + " | " + value + " |"
			}
		}
	}
	return strings.Join(lines, "\n")
}

// renderBlocking lists what a blocking verdict is waiting on, so a reader can
// go straight to the thing holding the pull request.
func renderBlocking(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	lines := make([]string, 0, len(reasons)+1)
	lines = append(lines, "This review is waiting on:")
	for _, reason := range reasons {
		lines = append(lines, "- "+reason)
	}
	return strings.Join(lines, "\n")
}

func formatReached(reached string) string {
	if strings.TrimSpace(reached) == "" {
		return "nothing"
	}
	return reached
}

func shortHead(head domain.HeadSHA) string {
	value := string(head)
	if len(value) <= shortHeadLength {
		return value
	}
	return value[:shortHeadLength]
}

func formatModels(models []string) string {
	if len(models) == 0 {
		return "unknown"
	}
	quoted := make([]string, 0, len(models))
	for _, model := range models {
		quoted = append(quoted, "`"+model+"`")
	}
	return strings.Join(quoted, ", ")
}

// formatDuration reports whole seconds, because finer precision tells a reader
// nothing about a review that takes seconds to minutes.
func formatDuration(duration time.Duration) string {
	seconds := max(int(duration.Round(time.Second)/time.Second), 0)
	if seconds == 1 {
		return "`1` second"
	}
	return fmt.Sprintf("`%d` seconds", seconds)
}

func formatYesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func formatCountAndImportances(findings []domain.Finding) string {
	if len(findings) == 0 {
		return "`0`"
	}
	return fmt.Sprintf("`%d` at importance %s", len(findings), formatFindingImportances(findings))
}

// RenderStartedBody renders the comment a run posts before it reads anything.
//
// It exists because the pull request said nothing at all until the first chunk
// came back, which on a large delta is minutes of silence with a pending check
// and no way to tell a slow review from one that never began. The comment is
// posted as soon as the run knows it is reviewing this head, and every later
// stage rewrites the same comment rather than adding another.
//
// It carries no review marker. Nothing has been reviewed yet, and a marker here
// would tell the next run this head was done.
func RenderStartedBody(head domain.HeadSHA) string {
	return strings.Join([]string{
		"## Review",
		"Reviewing `" + shortHead(head) + "`. This comment is rewritten when the review finishes.",
	}, "\n\n")
}

// RenderFailureBody renders the visible summary for a review that could not
// finish. It carries the same detail table as a successful review, reporting
// how far the review got, and it omits the review marker so the next attempt on
// the same head is not suppressed as already reviewed.
//
// Both title and detail are wording this service wrote. The provider's own
// message never reaches here, so neither is fenced or escaped: there is no
// untrusted text in this body to contain.
func RenderFailureBody(summary Summary, title string, detail string) string {
	summary.Failed = true
	parts := []string{"## Review", strings.TrimSpace(title)}
	if trimmedDetail := strings.TrimSpace(detail); trimmedDetail != "" {
		parts = append(parts, trimmedDetail)
	}
	if summary.ApprovalWithheld && summary.Decision != "" {
		summary.Decision = domain.ReviewDecisionComment
		parts = append(parts, renderVerdictSection(summary, false), RenderVerdictBody(summary))
	}
	parts = append(parts, RenderDetails(summary), marker.Summary())
	return strings.Join(parts, "\n\n")
}

// RenderSkipBody renders the visible notice for a delta the admission gate
// declined before any model call. It carries no review marker, because the
// gate never touches a review object and the head it names was never reviewed.
func RenderSkipBody(reason string) string {
	return strings.Join([]string{
		"## Review",
		"The review skipped this pull request because " + reason + ".",
	}, "\n\n")
}

// RenderProgressBody renders the visible comment between chunks, while the
// review is still running.
//
// It names what the review is already waiting on beside how much is left to
// read. A finding is posted as its chunk answers, so on a long delta the reader
// can start fixing the first one while the rest is still being read, instead of
// waiting for the whole run to say anything. The two facts arrive in either
// order and are shown together whenever both exist.
//
// It shares no renderer with the finished summary on purpose. The summary
// carries the review marker, which means this head was reviewed, and a comment
// describing an unfinished review must never say that.
func RenderProgressBody(head domain.HeadSHA, remaining int, waitingOn []string) string {
	progress := fmt.Sprintf("Reviewing `%s`. %s still to read.", shortHead(head), chunkCount(remaining))
	if remaining == 0 {
		progress = "Reviewing `" + shortHead(head) + "`. Every chunk has been read."
	}
	parts := []string{"## Review", progress}
	if waiting := renderBlocking(waitingOn); waiting != "" {
		parts = append(parts, waiting)
	}
	return strings.Join(parts, "\n\n")
}

// findingLocations names each published finding by the place a reader goes to
// act on it.
func findingLocations(published []domain.Finding) []string {
	locations := make([]string, 0, len(published))
	for _, finding := range published {
		normalizedPath, err := marker.NormalizePath(finding.Path)
		if err != nil {
			continue
		}
		locations = append(locations, fmt.Sprintf("%s:%d", codeSpan(normalizedPath), finding.EndLine))
	}
	return locations
}

// openThreadLocations names each finding of this service's own that the pull
// request is still waiting on, in the same shape a freshly published one takes.
func openThreadLocations(threads []githubapp.ReviewThread, botLogin string) []string {
	locations := make([]string, 0, len(threads))
	for _, thread := range threads {
		if thread.RootComment.Author != botLogin || thread.Resolved {
			continue
		}
		normalizedPath, err := marker.NormalizePath(thread.RootComment.Path)
		if err != nil {
			continue
		}
		locations = append(locations, fmt.Sprintf("%s:%d", codeSpan(normalizedPath), thread.RootComment.EndLine))
	}
	return locations
}

// mergeLocations joins two lists of places without naming one twice, keeping
// the order they were given in.
func mergeLocations(first []string, second []string) []string {
	merged := make([]string, 0, len(first)+len(second))
	seen := make(map[string]struct{}, len(first)+len(second))
	for _, list := range [][]string{first, second} {
		for _, location := range list {
			if _, found := seen[location]; found {
				continue
			}
			seen[location] = struct{}{}
			merged = append(merged, location)
		}
	}
	return merged
}

// codeSpan renders repository-controlled text as an inline code span that the
// text cannot break out of.
//
// A path is whatever the pull request named a file, so it reaches this comment
// under the service's own identity. A backtick would close the span and let the
// rest render as Markdown, and a line break would end the list item and let the
// remainder pose as the service's own prose, which is how a crafted filename
// puts a mention or a false verdict in a comment nobody would doubt.
func codeSpan(text string) string {
	// Every shape of line break becomes one, and that one becomes a space, so
	// the span stays on the line the service put it on. The reply formatter
	// already enumerates those shapes, and a second list of them would drift.
	flattened := strings.ReplaceAll(replyLineBreaks.Replace(text), "\n", " ")
	// A backtick inside a span closes it. The modifier letter at U+02CB renders
	// like one and closes nothing.
	return "`" + strings.ReplaceAll(flattened, "`", string(rune(0x2CB))) + "`"
}

// RenderIncompleteBody renders the visible comment for a pass that could not
// read every chunk it owed.
//
// It states what is left and that the next push covers it, and it carries no
// review marker for the same reason the progress body carries none. reason is
// this service's own wording for what went wrong; the provider's own sentence
// never reaches here, because a pull request comment is public and permanent.
//
// The table reports coverage rather than a stage. This run reached the end and
// published a verdict, so there is no stage it stopped at, and a chunk nobody
// read is exactly what the coverage row is for.
func RenderIncompleteBody(summary Summary, pending int, reason string, detail string) string {
	lead := fmt.Sprintf(
		"%s could not be reviewed on `%s`. The next push reviews %s.",
		chunkCount(pending),
		shortHead(summary.Head),
		chunkPronoun(pending),
	)
	blocking := summary.Blocking
	coverageReason := unreviewedHeadReason
	if reason == checkFailureUnavailable {
		lead = fmt.Sprintf(
			"%s could not be reviewed on `%s`. %s",
			chunkCount(pending),
			shortHead(summary.Head),
			reason,
		)
		blocking = replaceBlockingReason(blocking, unreviewedHeadReason, unreviewedProviderReason)
		coverageReason = unreviewedProviderReason
		reason = ""
	}
	parts := []string{
		"## Review",
		lead,
	}
	for _, note := range []string{reason, coverageReason, renderBlocking(blocking), detail} {
		if trimmed := strings.TrimSpace(note); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	parts = append(parts, RenderDetails(summary))
	return strings.Join(parts, "\n\n")
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
func chunkCount(count int) string {
	if count == 1 {
		return "1 chunk"
	}
	return fmt.Sprintf("%d chunks", count)
}

// chunkPronoun matches chunkCount, so the sentence around it agrees.
func chunkPronoun(count int) string {
	if count == 1 {
		return "it"
	}
	return "them"
}

// RenderInline renders anchored findings as GitHub inline review comments.
func RenderInline(head domain.HeadSHA, findings []domain.Finding) ([]githubapp.InlineComment, error) {
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
		body, err := marker.EncodeFindingBody(head, domain.Finding{
			Path:       normalizedPath,
			StartLine:  finding.StartLine,
			EndLine:    finding.EndLine,
			Title:      sanitizeProse(finding.Title),
			Body:       sanitizeProse(finding.Body),
			Evidence:   finding.Evidence,
			Claim:      finding.Claim,
			Suggestion: finding.Suggestion,
			Importance: finding.Importance,
		})
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

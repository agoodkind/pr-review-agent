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
	Head     domain.HeadSHA
	Decision domain.ReviewDecision
	// Blocking states what a requesting-changes verdict is waiting on, one
	// entry per cause. A block that names nothing reads as a silent repeat, so
	// a blocking verdict always carries at least one entry here.
	Blocking          []string
	Models            []string
	Duration          time.Duration
	FilesReviewed     int
	Chunks            int
	CoverageComplete  bool
	MinimumImportance int
	Observed          []domain.Finding
	Eligible          []domain.Finding
	Published         []domain.Finding
	PriorReviews      []reviewTrace
	Threads           []threadTrace
	// Reached names the last stage the review completed. A failed review fills
	// only the fields it got far enough to learn, so the reader can tell how far
	// it got rather than seeing zeros with no explanation.
	Reached string
	// Failed marks a review that stopped early, so the detail table reports
	// progress rather than a result.
	Failed bool
}

// Verdict states the outcome in one plain sentence.
//
// A block is not the same claim as a finding. The sentence used to be chosen
// from the decision alone, so every blocking verdict promised inline comments
// whether or not this run posted any. A head the run could not finish reading
// blocks with nothing inline, and one live review opened with "Severe findings
// are listed inline." above its own detail table reading "Findings published
// inline `0`", which sent the reader hunting for comments that were never
// written.
//
// The sentence therefore follows what reached the pull request. What holds a
// block with nothing inline is in the Waiting on list directly below, which
// renderBlocking writes and which a blocking verdict always carries at least one
// entry of, so the empty case points there rather than at nothing.
func (summary Summary) Verdict() string {
	if summary.Decision != domain.ReviewDecisionRequestChanges {
		return "No severe findings."
	}
	if len(summary.Published) > 0 {
		return "Severe findings are listed inline."
	}
	return "Changes are requested for the reasons listed below."
}

// Title names the outcome for the check run.
func (summary Summary) Title() string {
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
		[2]string{"Minimum importance", fmt.Sprintf("`%d`", summary.MinimumImportance)},
		[2]string{"Findings observed", formatCountAndImportances(summary.Observed)},
		[2]string{"Findings eligible", formatCountAndImportances(summary.Eligible)},
		[2]string{"Findings published inline", formatCountAndImportances(summary.Published)},
		[2]string{"Prior bot review IDs", formatReviewTraceIDs(summary.PriorReviews)},
		[2]string{"Bot thread IDs", formatThreadTraceIDs(summary.Threads)},
		[2]string{"Bot threads resolved", fmt.Sprintf("`%d`", countResolvedThreadTraces(summary.Threads))},
	)

	var builder strings.Builder
	builder.WriteString("<details>\n<summary>Review details</summary>\n\n")
	builder.WriteString("| | |\n| --- | --- |\n")
	for _, row := range rows {
		fmt.Fprintf(&builder, "| %s | %s |\n", row[0], row[1])
	}
	builder.WriteString("\n</details>")
	return builder.String()
}

// RenderBody renders the single visible GitHub review summary.
func RenderBody(summary Summary) string {
	parts := []string{"## Review", summary.Verdict()}
	if blocking := renderBlocking(summary.Blocking); blocking != "" {
		parts = append(parts, blocking)
	}
	parts = append(
		parts,
		RenderDetails(summary),
		marker.Summary()+"\n"+marker.Review(summary.Head),
	)
	return strings.Join(parts, "\n\n")
}

// blockingVerdictLead opens a blocking verdict body. It names the decision in
// this service's own words rather than reusing the summary's verdict sentence,
// because a body that repeats the comment is the thing this rendering exists to
// stop.
const blockingVerdictLead = "Changes requested."

// RenderVerdictBody renders the body of the review that carries the verdict.
//
// A verdict body must never restate the summary comment. Both used to open with
// the same "## Review" heading and the same verdict sentence, so one approving
// run published the identical text twice two seconds apart, and a reader saw two
// matching Review boxes stacked around the approval event. Dropping the detail
// table from this body halved the duplication and left the heading and the
// sentence, which is still a second box saying what the first one said.
//
// An approving verdict therefore carries no prose at all. The approval event is
// itself the message, and the comment above it already holds the summary and the
// detail. The review marker stays, and is the whole body: hasBotReviewMarker
// reads it to recognize a head this service has already reviewed, so a fully
// empty body would blind that gate for every approval. As an HTML comment it
// renders as nothing, which is the point.
//
// A blocking verdict keeps a body. One live blocking review carried only the
// marker, so it named nothing to fix and no edit could satisfy it. This body
// states the decision and what the block waits on, and nothing else, because
// everything else is already in the comment.
func RenderVerdictBody(summary Summary) string {
	if summary.Decision != domain.ReviewDecisionRequestChanges {
		return marker.Review(summary.Head)
	}
	parts := []string{blockingVerdictLead}
	if blocking := renderBlocking(summary.Blocking); blocking != "" {
		parts = append(parts, blocking)
	}
	parts = append(parts, marker.Review(summary.Head))
	return strings.Join(parts, "\n\n")
}

// renderVerdictRefreshProse is the summary comment prose for a verdict
// refreshed from thread state alone. It reports no run statistics because no
// model ran; the verdict and what it still waits on are the whole story.
func renderVerdictRefreshProse(summary Summary) string {
	parts := []string{"## Review", summary.Verdict()}
	if blocking := renderBlocking(summary.Blocking); blocking != "" {
		parts = append(parts, blocking)
	}
	parts = append(
		parts,
		"Verdict refreshed from review thread state on `"+shortHead(summary.Head)+"` with no new push.",
		marker.Summary()+"\n"+marker.Review(summary.Head),
	)
	return strings.Join(parts, "\n\n")
}

// renderBlocking lists what a blocking verdict is waiting on, so a reader can
// go straight to the thing holding the pull request.
func renderBlocking(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	lines := make([]string, 0, len(reasons)+1)
	lines = append(lines, "Waiting on:")
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
	parts = append(parts, RenderDetails(summary), marker.Summary())
	return strings.Join(parts, "\n\n")
}

// RenderSkipBody renders the visible notice for a delta the admission gate
// declined before any model call. It carries no review marker, because the
// gate never touches a review object and the head it names was never reviewed.
func RenderSkipBody(reason string) string {
	return strings.Join([]string{
		"## Review",
		"Review skipped: " + reason + ".",
	}, "\n\n")
}

// RenderProgressBody renders the visible comment between chunks, while the
// review is still running.
//
// It shares no renderer with the finished summary on purpose. The summary
// carries the review marker, which means this head was reviewed, and a comment
// describing an unfinished review must never say that.
func RenderProgressBody(head domain.HeadSHA, remaining int) string {
	if remaining == 0 {
		return strings.Join([]string{
			"## Review",
			"Reviewing `" + shortHead(head) + "`. Every chunk has been read.",
		}, "\n\n")
	}
	return strings.Join([]string{
		"## Review",
		fmt.Sprintf("Reviewing `%s`. %s still to read.", shortHead(head), chunkCount(remaining)),
	}, "\n\n")
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
	parts := []string{
		"## Review",
		fmt.Sprintf(
			"%s could not be reviewed on `%s`. The next push reviews %s.",
			chunkCount(pending),
			shortHead(summary.Head),
			chunkPronoun(pending),
		),
	}
	for _, note := range []string{reason, renderBlocking(summary.Blocking), detail} {
		if trimmed := strings.TrimSpace(note); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	parts = append(parts, RenderDetails(summary))
	return strings.Join(parts, "\n\n")
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
		// The evidence reaches the encoder, and no further. It is hashed into the
		// marker's claim key, which is what lets a later run recognize a reworded
		// restatement of this same claim; the rendered body still never prints it,
		// so the reader sees no quoted source line they already have beside the
		// comment.
		body, err := marker.EncodeFindingBody(head, domain.Finding{
			Path:       normalizedPath,
			StartLine:  finding.StartLine,
			EndLine:    finding.EndLine,
			Title:      sanitizeProse(finding.Title),
			Body:       sanitizeProse(finding.Body),
			Evidence:   finding.Evidence,
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

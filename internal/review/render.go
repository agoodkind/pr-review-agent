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

const (
	// minimumFenceLength is the shortest Markdown code fence.
	minimumFenceLength = 3
	// shortHeadLength is how much of a head SHA the review details show.
	shortHeadLength = 7
)

// Summary is everything one published review reports about itself. The visible
// comment and the check run both render from this one value, so the two can
// never disagree.
type Summary struct {
	Head              domain.HeadSHA
	Decision          domain.ReviewDecision
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
}

// Verdict states the outcome in one plain sentence.
func (summary Summary) Verdict() string {
	if summary.Decision == domain.ReviewDecisionRequestChanges {
		return "Severe findings are listed inline."
	}
	return "No severe findings."
}

// Title names the outcome for the check run.
func (summary Summary) Title() string {
	if summary.Decision == domain.ReviewDecisionRequestChanges {
		return "Changes requested"
	}
	return "Approved"
}

// RenderDetails renders the collapsed review detail table.
func RenderDetails(summary Summary) string {
	rows := [][2]string{
		{"Model", formatModels(summary.Models)},
		{"Duration", formatDuration(summary.Duration)},
		{"Head", "`" + shortHead(summary.Head) + "`"},
		{"Files reviewed", fmt.Sprintf("`%d`", summary.FilesReviewed)},
		{"Diff chunks", fmt.Sprintf("`%d`", summary.Chunks)},
		{"Coverage complete", formatYesNo(summary.CoverageComplete)},
		{"Minimum importance", fmt.Sprintf("`%d`", summary.MinimumImportance)},
		{"Findings observed", formatCountAndImportances(summary.Observed)},
		{"Findings eligible", formatCountAndImportances(summary.Eligible)},
		{"Findings published inline", formatCountAndImportances(summary.Published)},
		{"Prior bot review IDs", formatReviewTraceIDs(summary.PriorReviews)},
		{"Bot thread IDs", formatThreadTraceIDs(summary.Threads)},
		{"Bot threads resolved", fmt.Sprintf("`%d`", countResolvedThreadTraces(summary.Threads))},
	}

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
	return strings.Join([]string{
		"## Review",
		summary.Verdict(),
		RenderDetails(summary),
		marker.Summary() + "\n" + marker.Review(summary.Head),
	}, "\n\n")
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

// RenderFailureBody renders the visible summary for a review that could not finish.
// It omits the review marker so the next attempt on the same head is not
// suppressed as already reviewed.
func RenderFailureBody(title string, detail string) string {
	parts := []string{"## Review", strings.TrimSpace(title)}
	if trimmedDetail := strings.TrimSpace(detail); trimmedDetail != "" {
		fence := codeFenceFor(trimmedDetail)
		parts = append(parts, fence+"\n"+trimmedDetail+"\n"+fence)
	}
	parts = append(parts, marker.Summary())
	return strings.Join(parts, "\n\n")
}

// codeFenceFor returns a fence longer than any backtick run in the content, so
// a provider message containing backticks cannot break out of the block.
func codeFenceFor(content string) string {
	longestRun := 0
	currentRun := 0
	for _, character := range content {
		if character == '`' {
			currentRun++
			if currentRun > longestRun {
				longestRun = currentRun
			}
			continue
		}
		currentRun = 0
	}
	fenceLength := minimumFenceLength
	if longestRun >= fenceLength {
		fenceLength = longestRun + 1
	}
	return strings.Repeat("`", fenceLength)
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
		body, err := marker.EncodeFindingBody(head, domain.Finding{
			Path:       normalizedPath,
			StartLine:  finding.StartLine,
			EndLine:    finding.EndLine,
			Title:      sanitizeProse(finding.Title),
			Body:       sanitizeProse(finding.Body),
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

package review

import (
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

const unanchoredHeading = "## Unanchored findings"

// RenderBody renders the GitHub review body for one analysis result.
func RenderBody(head domain.HeadSHA, analysis Analysis) string {
	var builder strings.Builder

	summary := strings.TrimSpace(sanitizeProse(analysis.Summary))
	if summary != "" {
		builder.WriteString(summary)
	}

	if len(analysis.Unanchored) > 0 {
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(unanchoredHeading)
		builder.WriteString("\n\n")
		for index, finding := range analysis.Unanchored {
			if index > 0 {
				builder.WriteString("\n\n")
			}
			builder.WriteString(renderUnanchoredFinding(finding))
		}
	}

	if builder.Len() > 0 {
		builder.WriteString("\n\n")
	}
	builder.WriteString(marker.Review(head))
	return builder.String()
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

func renderUnanchoredFinding(finding domain.Finding) string {
	title := sanitizeProse(finding.Title)
	body := sanitizeProse(finding.Body)
	return fmt.Sprintf(
		"**%s** (%s:%d-%d)\n\n%s\n\nImportance: %d",
		title,
		finding.Path,
		finding.StartLine,
		finding.EndLine,
		body,
		finding.Importance,
	)
}

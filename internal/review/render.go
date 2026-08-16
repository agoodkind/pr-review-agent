package review

import (
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// RenderBody renders the single visible GitHub review summary.
func RenderBody(head domain.HeadSHA, decision domain.ReviewDecision) string {
	message := "No severe findings."
	if decision == domain.ReviewDecisionRequestChanges {
		message = "Severe findings are listed inline."
	}
	return "## Review\n\n" + message + "\n\n" + marker.Summary() + "\n" + marker.Review(head)
}

// RenderFailureBody renders the visible summary for a review that could not finish.
// It omits the review marker so the next attempt on the same head is not
// suppressed as already reviewed.
func RenderFailureBody(title string, detail string) string {
	parts := []string{"## Review", strings.TrimSpace(title)}
	if trimmedDetail := strings.TrimSpace(detail); trimmedDetail != "" {
		parts = append(parts, "```\n"+trimmedDetail+"\n```")
	}
	parts = append(parts, marker.Summary())
	return strings.Join(parts, "\n\n")
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

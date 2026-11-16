package review

import (
	"fmt"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
)

func TestRenderBodyPreservesModelAssessmentReasonsThatLookLikeTemplateNames(t *testing.T) {
	for _, reason := range []string{
		"output.missing-template",
		"output.file-budget-reason",
		"output.unreviewed-head-reason",
	} {
		t.Run(
			reason,
			func(t *testing.T) {
				var work deltaWork
				work.FileAssessments = []domain.FileAssessment{
					{
						Path:   "main.go",
						Action: domain.FileActionGap,
						Reason: reason,
					},
				}
				shortfall := classifyStructuralShortfall(work)
				var summary Summary
				summary.Decision = domain.ReviewDecisionComment
				summary.Omissions = shortfall.Hunks
				body, err := RenderBody(summary)
				if err != nil {
					t.Fatal(err)
				}

				visible, _, _ := strings.Cut(body, "<details>")
				if !strings.Contains(visible, reason) {
					t.Fatalf(
						"The summary replaced the model assessment reason %q: %s",
						reason,
						visible,
					)
				}
			},
		)
	}
}

func TestRenderBodyDisplaysStructuredOmissionCountAfterCheckpointReload(t *testing.T) {
	const additional = 5
	var original Summary
	original.Decision = domain.ReviewDecisionComment

	for index := range maximumListedUnreadHunks + additional {
		var hunk unreadHunk
		hunk.Path = fmt.Sprintf("file-%03d.go", index)
		hunk.Reason = "The review did not inspect this file."
		original.Omissions = append(original.Omissions, hunk)
	}

	checkpoint := RenderVerdictBody(original)
	var restored Summary
	restored.Decision = domain.ReviewDecisionComment
	restored.Omissions = decodeOmissionMarker(checkpoint)
	last := restored.Omissions[len(restored.Omissions)-1]
	if last.Omitted != additional+1 || last.Path != "" || last.Reason != "" {
		t.Fatalf("The checkpoint must store the omitted count without display prose: %+v", last)
	}

	body, err := RenderBody(restored)
	if err != nil {
		t.Fatal(err)
	}

	visible, _, _ := strings.Cut(body, "<details>")
	if !strings.Contains(visible, "Additional omissions") ||
		!strings.Contains(visible, "6 more not listed here") {
		t.Fatalf("The summary omitted the checkpoint's remaining file count: %s", visible)
	}
}

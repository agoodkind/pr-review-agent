package review

import (
	"fmt"
	"html"
	"slices"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

func renderFileAssessments(assessments []domain.FileAssessment, maximumBytes int) string {
	if len(assessments) == 0 || maximumBytes <= 0 {
		return ""
	}
	ordered := slices.Clone(assessments)
	slices.SortFunc(ordered, func(left, right domain.FileAssessment) int {
		return strings.Compare(left.Path, right.Path)
	})
	var builder strings.Builder
	builder.WriteString("#### File assessments\n\nThe table records the assessment scope for each classified file.\n\n| File | Content | Assessment | Content review requirement | Reason | Evidence |\n| --- | --- | --- | --- | --- | --- |\n")
	listed := 0
	const noticeAllowance = 256
	for _, assessment := range ordered[:min(len(ordered), maximumListedUnreadHunks)] {
		row := fmt.Sprintf("| %s | %s | %s | %s | %s | %s |\n",
			assessmentTableCell(assessment.Path), assessmentTableCell(string(assessment.ContentKind)),
			assessmentTableCell(string(assessment.Action)), assessmentTableCell(string(assessment.ContentReviewRequirement)), assessmentTableCell(assessment.Reason),
			assessmentTableCell(strings.Join(assessment.Evidence, ", ")))
		if builder.Len()+len(row)+noticeAllowance > maximumBytes {
			break
		}
		builder.WriteString(row)
		listed++
	}
	if len(ordered) > listed {
		fmt.Fprintf(&builder, "\nThe summary displays %d of %d file assessments.\n", listed, len(ordered))
	}
	builder.WriteString("\n")
	if builder.Len() > maximumBytes {
		return ""
	}
	return builder.String()
}

func assessmentTableCell(value string) string {
	value = strings.Join(strings.Fields(sanitizeProse(value)), " ")
	value = html.EscapeString(value)
	return strings.ReplaceAll(value, "|", "&#124;")
}

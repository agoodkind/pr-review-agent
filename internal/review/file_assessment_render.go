package review

import (
	"html"
	"reflect"
	"slices"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

func renderFileAssessments(assessments []domain.FileAssessment, maximumBytes int) (string, error) {
	var state renderState

	if len(assessments) == 0 || maximumBytes <= 0 {
		return state.finish("")
	}

	ordered := slices.Clone(assessments)
	slices.SortFunc(
		ordered,
		func(left, right domain.FileAssessment) int {
			return strings.Compare(left.Path, right.Path)
		},
	)

	var builder strings.Builder
	builder.WriteString(
		state.capture(renderOutputTemplate("output.file-assessment-header", reflect.ValueOf(nil))),
	)

	listed := 0

	const noticeAllowance = 256

	for _, assessment := range ordered[:min(len(ordered), maximumListedUnreadHunks)] {
		row := state.capture(
			renderOutputTemplate(
				"output.file-assessment-row",
				reflect.ValueOf([]string{
					assessmentTableCell(assessment.Path),
					assessmentTableCell(string(assessment.ContentKind)),
					assessmentTableCell(string(assessment.Action)),
					assessmentTableCell(string(assessment.ContentReviewRequirement)),
					assessmentTableCell(assessment.Reason),
					assessmentTableCell(
						state.capture(
							renderOutputTemplate(
								"output.comma_list",
								reflect.ValueOf(assessment.Evidence),
							),
						),
					),
				}),
			),
		)
		if builder.Len()+len(row)+noticeAllowance > maximumBytes {
			break
		}

		builder.WriteString(row)

		listed++
	}

	if len(ordered) > listed {
		builder.WriteString(
			state.capture(
				renderOutputTemplate(
					"output.file-assessment-count",
					reflect.ValueOf([]int{listed, len(ordered)}),
				),
			),
		)
	}

	builder.WriteString(
		state.capture(renderOutputTemplate("output.file-assessment-end", reflect.ValueOf(nil))),
	)

	if builder.Len() > maximumBytes {
		return state.finish("")
	}

	return state.finish(builder.String())
}

func assessmentTableCell(value string) string {
	value = strings.Join(strings.Fields(sanitizeProse(value)), " ")
	value = html.EscapeString(value)

	return strings.ReplaceAll(value, "|", "&#124;")
}

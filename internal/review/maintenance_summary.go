package review

import (
	"fmt"
	"reflect"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

const (
	maintenanceFailureStart = "<!-- pr-review-agent:maintenance-failure:start -->"
	maintenanceFailureEnd   = "<!-- pr-review-agent:maintenance-failure:end -->"
	reviewDetailsStart      = "<details>\n<summary>Review details</summary>"
)

func completedHeadState(state marker.State, head domain.HeadSHA) bool {
	return state.LastReviewed == head && len(state.Pending) == 0 && len(state.Unread) == 0
}

func preserveCheckpointSummary(state marker.State, body string) bool {
	_, reviewed := marker.FindReview(body)

	return reviewed || len(state.Completed) > 0 || len(state.Pending) > 0 ||
		strings.Contains(
			body,
			metadataRecordPrefix,
		) || strings.Contains(body, fileAssessmentMarkerPrefix)
}

func refreshFailureResult(body, result string) (string, error) {
	var rendering renderState
	start := strings.Index(body, reviewDetailsStart)
	if start < 0 {
		return rendering.finish(body)
	}

	row := rendering.capture(
		renderOutputTemplate(
			"output.result_row",
			reflect.ValueOf(rendering.capture(rawResultCell(result))),
		),
	)

	lines := strings.Split(body[start:], "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "| `result` |") {
			lines[index] = row

			return rendering.finish(body[:start] + strings.Join(lines, "\n"))
		}
	}

	for index, line := range lines {
		if strings.HasPrefix(line, "| --- |") {
			lines[index] += "\n" + row

			return rendering.finish(body[:start] + strings.Join(lines, "\n"))
		}
	}

	return rendering.finish(body)
}

func renderMaintenanceFailure(body, title, detail string) (string, error) {
	var rendering renderState
	notice := rendering.capture(
		renderOutputTemplate(
			"output.maintenance_notice",
			reflect.ValueOf(
				struct {
					Title  string
					Detail string
				}{
					Title:  rendering.capture(renderOutputText(title)),
					Detail: detail,
				},
			),
		),
	)

	return rendering.finish(
		rendering.capture(
			renderCheckpointNotice(rendering.capture(stripActiveReviewProgress(body)), notice),
		),
	)
}

func stripActiveReviewProgress(body string) (string, error) {
	var rendering renderState
	prefix, remaining, found := strings.Cut(body, "\n\n")
	if !found || strings.TrimSpace(prefix) != "## Review" {
		return rendering.finish(body)
	}

	progress, remaining, found := strings.Cut(remaining, "\n\n")
	if !strings.HasPrefix(progress, "Reviewing `") || strings.Contains(progress, "\n") {
		return rendering.finish(body)
	}

	if !found {
		return rendering.finish(prefix)
	}

	return rendering.finish(rendering.capture(renderOutputParts(prefix, remaining)))
}

func renderCheckpointNotice(body, notice string) (string, error) {
	var rendering renderState
	body = stripMaintenanceFailure(stripReassessmentNotice(stripDurableStateMarker(body)))

	return rendering.finish(
		rendering.capture(
			renderOutputTemplate(
				"output.checkpoint_notice",
				reflect.ValueOf(
					struct {
						Body   string
						Notice string
					}{
						Body:   body,
						Notice: notice,
					},
				),
			),
		),
	)
}

func stripMaintenanceFailure(body string) string {
	start := strings.Index(body, maintenanceFailureStart)
	if start < 0 {
		return body
	}

	end := strings.Index(body[start:], maintenanceFailureEnd)
	if end < 0 {
		return body
	}

	return strings.TrimSpace(body[:start] + body[start+end+len(maintenanceFailureEnd):])
}

// ClearUpdateFailure leaves the verdict and checkpoint unchanged.
func ClearUpdateFailure(body string) string {
	return stripReassessmentNotice(stripMaintenanceFailure(body))
}

func preservedReviewDetails(body string, summary Summary) (string, error) {
	var rendering renderState
	start, end, err := reviewDetailsRange(body, "completed review summary")
	if err == nil {
		return rendering.finish(rendering.capture(refreshThreadDetails(body[start:end], summary)))
	}

	return rendering.finish(
		rendering.capture(
			renderOutputTemplate(
				"output.legacy_details",
				reflect.ValueOf(
					struct {
						Head       string
						Coverage   string
						Scope      string
						Importance int
						Threads    string
						Open       int
						Resolved   int
					}{
						Head:     shortHead(summary.Head),
						Coverage: rendering.capture(formatYesNo(summary.CoverageComplete)),
						Scope: rendering.capture(
							formatYesNo(summary.CoverageComplete || summary.OmissionsAccepted),
						),
						Importance: summary.MinimumImportance,
						Threads:    rendering.capture(formatThreadTraceIDs(summary.Threads)),
						Open:       countOpenThreadTraces(summary.Threads),
						Resolved:   countResolvedThreadTraces(summary.Threads),
					},
				),
			),
		),
	)
}

func preservedReportPrefix(body string) (string, error) {
	var rendering renderState
	if start := strings.Index(body, reportSectionStart); start >= 0 {
		if end := strings.Index(body[start:], reportSectionEnd); end >= 0 {
			return rendering.finish(
				rendering.capture(
					renderOutputParts(
						rendering.capture(
							renderOutputTemplate("output.review_heading", reflect.ValueOf(nil)),
						),
						body[start:start+end+len(reportSectionEnd)],
					),
				),
			)
		}
	}

	end := strings.Index(body, verdictSectionStart)
	if end < 0 {
		end = strings.Index(body, "### Verdict")
	}

	if end < 0 {
		start, _, err := reviewDetailsRange(body, "completed review summary")
		if err == nil {
			end = start
		}
	}

	if end >= 0 {
		prefix := body[:end]
		if boundary := strings.Index(prefix, "<!-- pr-review-agent:"); boundary >= 0 {
			prefix = prefix[:boundary]
		}

		prefix = strings.TrimSpace(prefix)
		if strings.Contains(prefix, "### Summary") || strings.Contains(prefix, "### Changes") {
			return rendering.finish(prefix)
		}
	}

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.review_heading", reflect.ValueOf(nil))),
	)
}

// MetadataMatches checks the revision because title and description edits can keep the commit unchanged.
func MetadataMatches(body string, head domain.HeadSHA, revision string) bool {
	record, found, err := decodeMetadataRecord(body)
	return err == nil && found && record.Head == head && record.Revision == revision
}

// RestoreReviewDetails preserves the destination verdict and checkpoint when copying source details.
func RestoreReviewDetails(
	existingBody string,
	sourceDetails string,
	threads []githubapp.ReviewThread,
	botLogin string,
) (string, error) {
	var rendering renderState
	start, end, err := reviewDetailsRange(existingBody, "existing summary")
	if err != nil {
		return "", err
	}

	sourceStart, sourceEnd, err := reviewDetailsRange(sourceDetails, "source check summary")
	if err != nil {
		return "", err
	}

	var summary Summary

	summary.Threads = traceThreads(threads, botLogin)

	details := rendering.capture(
		refreshThreadDetails(sourceDetails[sourceStart:sourceEnd], summary),
	)
	if existingBody[start:end] == details {
		return rendering.finish(existingBody)
	}

	return rendering.finish(existingBody[:start] + details + existingBody[end:])
}

func reviewDetailsRange(body, source string) (int, int, error) {
	if strings.Count(body, "<summary>Review details</summary>") != 1 ||
		strings.Count(body, reviewDetailsStart) != 1 {
		return 0, 0, fmt.Errorf("%s requires exactly one review details section", source)
	}

	start := strings.Index(body, reviewDetailsStart)
	cursor := start + len("<details>")

	depth := 1
	for depth > 0 {
		closing := strings.Index(body[cursor:], "</details>")
		if closing < 0 {
			return 0, 0, fmt.Errorf("%s contains an incomplete review details section", source)
		}

		opening := strings.Index(body[cursor:], "<details")
		if opening >= 0 && opening < closing {
			openingEnd := strings.IndexByte(body[cursor+opening:], '>')
			if openingEnd < 0 || opening+openingEnd >= closing {
				return 0, 0, fmt.Errorf("%s contains an incomplete nested details section", source)
			}

			cursor += opening + openingEnd + 1
			depth++

			continue
		}

		cursor += closing + len("</details>")
		depth--
	}

	return start, cursor, nil
}

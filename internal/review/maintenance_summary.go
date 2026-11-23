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

func refreshRetryRows(body string, outcome domain.AssessmentOutcome) (string, error) {
	start := strings.Index(body, reviewDetailsStart)
	if start < 0 {
		return body, nil
	}

	start, end, err := reviewDetailsRange(body, "saved summary")
	if err != nil {
		return "", err
	}

	retryable, retryAt := retryCells(outcome)
	var rendered renderState
	lines := strings.Split(body[start:end], "\n")

	for _, item := range []rawDetailRow{
		{Key: "retryable", Value: retryable},
		{Key: "retry_at", Value: retryAt},
	} {
		key := rendered.capture(rawTableCell(item.Key))
		value := rendered.capture(rawTableCell(item.Value))
		row := rendered.capture(
			renderOutputTemplate(
				"output.detail_row",
				reflect.ValueOf(rawDetailRow{
					Key:   key,
					Value: value,
				}),
			),
		)
		found := false

		for index, line := range lines {
			if strings.HasPrefix(line, "| `"+item.Key+"` |") {
				lines[index] = row
				found = true

				break
			}
		}

		if !found {
			for index, line := range lines {
				if strings.HasPrefix(line, "| --- |") {
					lines[index] += "\n" + row

					break
				}
			}
		}
	}

	return rendered.finish(body[:start] + strings.Join(lines, "\n") + body[end:])
}

func refreshFailureDetails(body string, summary Summary, detail string) (string, error) {
	updated, err := refreshFailureResult(body, summary.Result)
	if err != nil {
		return "", err
	}

	updated, err = refreshRetryRows(updated, summary.Outcome)
	if err != nil || !strings.Contains(updated, reviewDetailsStart) {
		return updated, err
	}

	start, end, err := reviewDetailsRange(updated, "saved summary")
	if err != nil {
		return "", err
	}

	details := stripFailureDiagnostics(updated[start:end])

	if strings.TrimSpace(detail) != "" {
		block, renderErr := renderOutputTemplate(
			"output.failure_diagnostics",
			reflect.ValueOf(detail),
		)
		if renderErr != nil {
			return "", renderErr
		}

		details = strings.TrimSuffix(details, "</details>") + block + "\n\n</details>"
	}

	return updated[:start] + details + updated[end:], nil
}

func renderMaintenanceFailure(body string, outcome domain.AssessmentOutcome) (string, error) {
	var rendering renderState
	notice := rendering.capture(renderFailureNotice(outcome))

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
	active := strings.HasPrefix(progress, "PR Review Agent is reviewing `") ||
		strings.HasPrefix(progress, "Reviewing `")
	if !active || strings.Contains(progress, "\n") {
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
	return stripFailureDiagnostics(stripReassessmentNotice(stripMaintenanceFailure(body)))
}

func stripFailureDiagnostics(body string) string {
	const start = "<!-- pr-review-agent:failure-diagnostics:start -->"
	const end = "<!-- pr-review-agent:failure-diagnostics:end -->"
	first := strings.Index(body, start)
	if first < 0 {
		return body
	}

	last := strings.Index(body[first:], end)
	if last < 0 {
		return body
	}

	return body[:first] + body[first+last+len(end):]
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

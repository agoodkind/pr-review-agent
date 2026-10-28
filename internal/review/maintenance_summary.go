package review

import (
	"fmt"
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
	return reviewed || len(state.Completed) > 0 || strings.Contains(body, reviewDetailsStart) ||
		strings.Contains(body, metadataRecordPrefix) || strings.Contains(body, fileAssessmentMarkerPrefix)
}

func renderMaintenanceFailure(body, title, detail string) string {
	return renderCheckpointNotice(stripActiveReviewProgress(body), "### "+title+"\n\n"+detail)
}

func stripActiveReviewProgress(body string) string {
	prefix, remaining, found := strings.Cut(body, "\n\n")
	if !found || strings.TrimSpace(prefix) != "## Review" {
		return body
	}
	progress, remaining, found := strings.Cut(remaining, "\n\n")
	if !strings.HasPrefix(progress, "Reviewing `") || strings.Contains(progress, "\n") {
		return body
	}
	if !found {
		return prefix
	}
	return prefix + "\n\n" + remaining
}

func renderCheckpointNotice(body, notice string) string {
	body = stripMaintenanceFailure(stripReassessmentNotice(stripDurableStateMarker(body)))
	return body + "\n\n" + strings.Join([]string{
		maintenanceFailureStart,
		notice,
		maintenanceFailureEnd,
	}, "\n\n")
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

func preservedReviewDetails(body string, summary Summary) string {
	start, end, err := reviewDetailsRange(body, "completed review summary")
	if err == nil {
		return refreshThreadDetails(body[start:end], summary)
	}
	return fmt.Sprintf(
		"%s\n\nThe completed review statistics are unavailable.\n\n| | |\n| --- | --- |\n| Head | `%s` |\n| All file content inspected | %s |\n| Review scope assessed | %s |\n| Minimum importance | `%d` |\n| Bot thread IDs | %s |\n| Bot threads open | `%d` |\n| Bot threads resolved | `%d` |\n\n</details>",
		reviewDetailsStart, shortHead(summary.Head), formatYesNo(summary.CoverageComplete), formatYesNo(summary.CoverageComplete || summary.OmissionsAccepted), summary.MinimumImportance,
		formatThreadTraceIDs(summary.Threads), countOpenThreadTraces(summary.Threads), countResolvedThreadTraces(summary.Threads),
	)
}

func preservedReportPrefix(body string) string {
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
			return prefix
		}
	}
	return "## Review"
}

// MetadataMatches checks the revision because title and description edits can keep the commit unchanged.
func MetadataMatches(body string, head domain.HeadSHA, revision string) bool {
	record, found, err := decodeMetadataRecord(body)
	return err == nil && found && record.Head == head && record.Revision == revision
}

// RestoreReviewDetails preserves the destination verdict and checkpoint when copying source details.
func RestoreReviewDetails(existingBody, sourceDetails string, threads []githubapp.ReviewThread, botLogin string) (string, error) {
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
	details := refreshThreadDetails(sourceDetails[sourceStart:sourceEnd], summary)
	if existingBody[start:end] == details {
		return existingBody, nil
	}
	return existingBody[:start] + details + existingBody[end:], nil
}

func reviewDetailsRange(body, source string) (int, int, error) {
	if strings.Count(body, "<summary>Review details</summary>") != 1 || strings.Count(body, reviewDetailsStart) != 1 {
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

package review

import (
	"reflect"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func renderSummaryFindings(summary Summary) (string, error) {
	var state renderState

	var parts []string
	if fallback := state.capture(
		renderFallbackFindings(unpublishedSummaryFindings(summary, summary.Fallback)),
	); fallback != "" {
		parts = append(parts, fallbackSectionStart+"\n"+fallback+"\n"+fallbackSectionEnd)
	}

	return state.finish(strings.Join(parts, "\n\n"))
}

func renderFindingCheckpoints(summary Summary) string {
	var parts []string
	if summary.MetadataRevision != "" {
		parts = append(parts, encodeMetadataRecord(metadataRecordFromSummary(summary)))
	}

	if fallback := encodeFallbackRecord(summary); fallback != "" {
		parts = append(parts, fallback)
	}

	return strings.Join(parts, "\n")
}

func unpublishedSummaryFindings(summary Summary, findings []domain.Finding) []domain.Finding {
	published := make(map[string]struct{}, len(summary.Published)+len(summary.Threads))
	for _, finding := range summary.Published {
		if id, err := marker.FindingID(finding); err == nil {
			published[id] = struct{}{}
		}
	}

	for _, thread := range summary.Threads {
		if thread.FindingID != "" {
			published[thread.FindingID] = struct{}{}
		}
	}

	remaining := make([]domain.Finding, 0, len(findings))
	for _, finding := range findings {
		id, err := marker.FindingID(finding)
		if err == nil {
			if _, found := published[id]; found {
				continue
			}
		}

		remaining = append(remaining, finding)
	}

	return remaining
}

func renderReviewMetadata(summary Summary) (string, error) {
	var state renderState

	if summary.MetadataFindingsDisplay == reviewrules.FindingsHidden {
		return state.finish("")
	}

	body := state.capture(
		renderMetadataFindings(unpublishedSummaryFindings(summary, summary.Metadata)),
	)
	if body == "" {
		return state.finish("")
	}

	return state.finish(
		state.capture(
			renderOutputTemplate(
				"output.metadata-findings",
				reflect.ValueOf(
					struct {
						Collapsed bool
						Body      string
					}{
						summary.MetadataFindingsDisplay == reviewrules.FindingsCollapsed,
						body,
					},
				),
			),
		),
	)
}

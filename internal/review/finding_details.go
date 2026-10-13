package review

import (
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

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

func renderSummaryMetadata(summary Summary) string {
	if summary.MetadataFindingsDisplay == reviewrules.FindingsHidden {
		return ""
	}
	body := renderMetadataFindings(unpublishedSummaryFindings(summary, summary.Metadata))
	if body == "" {
		return ""
	}
	if summary.MetadataFindingsDisplay == reviewrules.FindingsCollapsed {
		return "<details>\n<summary>Prose findings</summary>\n\n" + body + "\n\n</details>"
	}
	return "#### Prose findings\n\n" + body
}

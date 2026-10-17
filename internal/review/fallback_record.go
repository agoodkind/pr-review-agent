package review

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

const fallbackRecordPrefix = "<!-- pr-review-agent:fallback-record:v1 "

type fallbackReviewRecord struct {
	Deduplication map[reviewrules.ID]reviewrules.Deduplication `json:"deduplication,omitempty"`
	Head          domain.HeadSHA                               `json:"head"`
	Findings      []domain.Finding                             `json:"findings"`
}

func encodeFallbackRecord(summary Summary) string {
	if len(summary.Fallback) == 0 {
		return ""
	}
	record := fallbackReviewRecord{Head: summary.Head, Findings: append([]domain.Finding(nil), summary.Fallback...), Deduplication: make(map[reviewrules.ID]reviewrules.Deduplication)}
	for index := range record.Findings {
		finding := record.Findings[index]
		if finding.Deduplication == reviewrules.DeduplicateLocation {
			record.Deduplication[finding.RuleID] = finding.Deduplication
		}
		record.Findings[index].Evidence = ""
		record.Findings[index].Claim = ""
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		slog.Error("Fallback review state encoding failed", "err", err)
		return ""
	}
	return fallbackRecordPrefix + base64.RawURLEncoding.EncodeToString(encoded) + " -->"
}

func readFallbackRecord(body string, head domain.HeadSHA) (fallbackReviewRecord, bool) {
	var record fallbackReviewRecord
	start := strings.LastIndex(body, fallbackRecordPrefix)
	if start < 0 {
		return record, false
	}
	payload, _, found := strings.Cut(body[start+len(fallbackRecordPrefix):], " -->")
	if !found {
		slog.Warn("Fallback review state is incomplete")
		return record, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		slog.Warn("Fallback review state decoding failed", "err", err)
		return record, false
	}
	if err := json.Unmarshal(decoded, &record); err != nil {
		slog.Warn("Fallback review state parsing failed", "err", err)
		return record, false
	}
	if _, err := domain.ParseHeadSHA(string(record.Head)); err != nil || record.Head != head {
		return record, false
	}
	for index, finding := range record.Findings {
		mode := record.Deduplication[finding.RuleID]
		if mode != "" && mode != reviewrules.DeduplicateDefect && mode != reviewrules.DeduplicateLocation {
			slog.Warn("Fallback review state contains an invalid deduplication policy")
			return record, false
		}
		record.Findings[index].Deduplication = mode
		if finding.EffectiveSurface() != domain.FindingFile || finding.Validate() != nil {
			slog.Warn("Fallback review state contains an invalid finding")
			return record, false
		}
		if _, err := marker.FindingID(finding); err != nil {
			slog.Warn("Fallback review state contains an invalid path", "err", err)
			return record, false
		}
	}
	return record, true
}

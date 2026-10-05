package review

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

const metadataRecordPrefix = "<!-- pr-review-agent:metadata:v1 "

var errMetadataChanged = errors.New("pull request prose requires a current review")

type metadataReviewRecord struct {
	Head              domain.HeadSHA   `json:"head"`
	Revision          string           `json:"revision"`
	Findings          []domain.Finding `json:"findings"`
	Omissions         []unreadHunk     `json:"omissions,omitempty"`
	OmissionsAccepted bool             `json:"omissions_accepted,omitempty"`
	DecisionReason    string           `json:"decision_reason,omitempty"`
}

func encodeMetadataRecord(record metadataReviewRecord) string {
	record.Findings = append([]domain.Finding(nil), record.Findings...)
	for index := range record.Findings {
		record.Findings[index].Evidence = ""
		record.Findings[index].Claim = ""
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		slog.Error("Metadata review state encoding failed", "err", err)
		return ""
	}
	return metadataRecordPrefix + base64.RawURLEncoding.EncodeToString(encoded) + " -->"
}

func decodeMetadataRecord(body string) (metadataReviewRecord, bool, error) {
	var record metadataReviewRecord
	start := strings.LastIndex(body, metadataRecordPrefix)
	if start < 0 {
		return record, false, nil
	}
	payload, _, found := strings.Cut(body[start+len(metadataRecordPrefix):], " -->")
	if !found {
		return record, false, errors.New("the metadata review state is incomplete")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return record, false, errors.New("the metadata review state cannot be decoded")
	}
	if err := json.Unmarshal(decoded, &record); err != nil {
		return record, false, errors.New("the metadata review state is invalid")
	}
	if _, err := domain.ParseHeadSHA(string(record.Head)); err != nil || record.Revision == "" {
		return record, false, errors.New("the metadata review revision is invalid")
	}
	for _, finding := range record.Findings {
		if finding.EffectiveSurface() == domain.FindingFile || finding.Validate() != nil {
			return record, false, errors.New("the stored metadata finding is invalid")
		}
	}
	return record, true, nil
}

func (service *Service) readMetadataRecord(ctx context.Context, job domain.ReviewJob) (metadataReviewRecord, bool, error) {
	var empty metadataReviewRecord
	comments, err := service.github.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		slog.WarnContext(ctx, "Read metadata review state failed", "err", err)
		return empty, false, fmt.Errorf("read metadata review state: %w", err)
	}
	existing, found := findSummaryComment(comments, service.botLogin)
	if !found {
		return empty, false, nil
	}
	return decodeMetadataRecord(existing.Body)
}

func (service *Service) restoreCompletedMetadata(ctx context.Context, job domain.ReviewJob, pullRequest githubapp.PullRequest, chunks []diff.Chunk, completed []string) ([]domain.Finding, []string, error) {
	record, found, err := service.readMetadataRecord(ctx, job)
	if err != nil {
		return nil, completed, err
	}
	if found && record.Head == job.Head && record.Revision == diff.PullRequestRevision(pullRequest) {
		return record.Findings, completed, nil
	}
	for _, chunk := range chunks {
		if len(chunk.Metadata) > 0 {
			completed = removeChunkID(completed, chunkID(chunk))
		}
	}
	return nil, completed, nil
}

func renderMetadataFindings(findings []domain.Finding) string {
	if len(findings) == 0 {
		return ""
	}
	ordered := append([]domain.Finding(nil), findings...)
	sortFindings(ordered)
	sections := []string{"#### Prose findings"}
	for _, finding := range ordered {
		source := diff.MetadataSource{Surface: finding.EffectiveSurface(), CommitSHA: finding.CommitSHA, Text: "", StartLine: 0, EndLine: 0}
		label := source.Label()
		parts := []string{
			fmt.Sprintf("##### %s, lines %d-%d: %s", label, finding.StartLine, finding.EndLine, sanitizeReportText(finding.Title)),
			sanitizeReportText(finding.Body),
		}
		if finding.CorrectionAction == domain.CorrectionDelete {
			parts = append(parts, fmt.Sprintf("Delete lines %d-%d.", finding.StartLine, finding.EndLine))
		} else {
			fence := metadataReplacementFence(finding.Suggestion)
			parts = append(parts, "Replace those lines with:", fence+"text\n"+finding.Suggestion+"\n"+fence)
		}
		sections = append(sections, strings.Join(parts, "\n\n"))
	}
	return strings.Join(sections, "\n\n")
}

func metadataReplacementFence(text string) string {
	maximum, current := 2, 0
	for _, character := range text {
		if character == '`' {
			current++
			maximum = max(maximum, current)
		} else {
			current = 0
		}
	}
	return strings.Repeat("`", maximum+1)
}

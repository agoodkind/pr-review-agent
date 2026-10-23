package review

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

var (
	nativeHiddenMarkup = regexp.MustCompile(`(?s)<!--.*?(?:-->|$)|<sub>.*?</sub>`)
	nativeCodeBlock    = regexp.MustCompile("(?s)```.*?(?:```|$)|~~~.*?(?:~~~|$)")
	nativeHeading      = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]+[^\n]*(?:\n|$)`)
	nativeHTMLFormat   = regexp.MustCompile(`</?(?:i|em|strong|b|p|br|code)>`)
)

func (service *Service) nativeRequestedChangesBody(ctx context.Context, job domain.ReviewJob, summary Summary, commentID int64) (string, error) {
	reason, err := service.nativeBlockingReason(ctx, job, summary)
	if err != nil {
		return "", err
	}
	var data reviewrules.PromptData
	data.Input = reason
	data.SummaryURL = nativeSummaryURL(job.Repository, job.Number, commentID)
	body, err := renderPrompt(service.reviewPolicy, "native.request_changes", data)
	if err != nil {
		return "", err
	}
	if !strings.Contains(body, reason) {
		return "", errors.New("native.request_changes must include the blocking explanation")
	}
	return body + "\n\n" + RenderVerdictBody(summary), nil
}

func (service *Service) nativeBlockingReason(ctx context.Context, job domain.ReviewJob, summary Summary) (string, error) {
	logger := gklog.L(ctx)
	candidates := make([]domain.Finding, 0, len(summary.Metadata))
	for _, finding := range summary.Metadata {
		if finding.EffectiveSurface() == domain.FindingFile || finding.Validate() != nil {
			return "", errors.New("native rejection requires an actionable metadata finding")
		}
		candidates = append(candidates, finding)
	}
	threads, err := service.github.ListReviewThreads(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.WarnContext(ctx, "Read native rejection findings failed", "err", err)
		return "", fmt.Errorf("read native rejection findings: %w", err)
	}
	var footerData reviewrules.PromptData
	footer, err := renderPrompt(service.reviewPolicy, "inline.footer", footerData)
	if err != nil {
		return "", err
	}
	for _, thread := range threads {
		if !actionableBotThread(thread, service.botLogin) {
			continue
		}
		_, finding, decodeErr := marker.DecodeFindingBody(thread.RootComment)
		if decodeErr != nil {
			finding.Path = thread.RootComment.Path
			finding.StartLine, finding.EndLine = thread.RootComment.StartLine, thread.RootComment.EndLine
			finding.Body = nativeLegacyExplanation(thread.RootComment.Body, footer)
			finding.Importance = domain.MinimumFindingImportance
			if recorded, found := marker.FindFinding(thread.RootComment.Body); found {
				finding.Importance = recorded.Importance
			}
		}
		candidates = append(candidates, finding)
	}
	var selected domain.Finding
	for _, finding := range candidates {
		if strings.TrimSpace(finding.Body) == "" {
			continue
		}
		if selected.Body == "" || finding.Importance > selected.Importance || finding.Importance == selected.Importance && compareFindings(finding, selected) < 0 {
			selected = finding
		}
	}
	if selected.Body == "" {
		return "", errors.New("native rejection requires a readable blocking finding explanation")
	}
	sentence := firstReportSentences(sanitizeReportProse(selected.Body), 1)
	if validateFullSentence("native rejection explanation", sentence) != nil {
		sentence += "."
	}
	return sentence, nil
}

func nativeLegacyExplanation(body, footer string) string {
	body = strings.ReplaceAll(body, footer, "")
	body = replyLineBreaks.Replace(body)
	body = nativeHiddenMarkup.ReplaceAllString(body, "")
	body = nativeCodeBlock.ReplaceAllString(body, "")
	body = nativeHeading.ReplaceAllString(body, "")
	return nativeHTMLFormat.ReplaceAllString(body, "")
}

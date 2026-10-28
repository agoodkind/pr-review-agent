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
	data, err := service.nativeBlockingReason(ctx, job, summary, nativeSummaryURL(job.Repository, job.Number, commentID))
	if err != nil {
		return "", err
	}
	body, err := renderPrompt(service.reviewPolicy, "native.request_changes", data)
	if err != nil {
		return "", err
	}
	if !strings.Contains(body, data.Input) || !strings.Contains(body, data.SummaryURL) {
		return "", errors.New("native.request_changes must include the finding label and permalink")
	}
	return body + "\n\n" + RenderVerdictBody(summary), nil
}

type nativeBlockingFinding struct {
	finding domain.Finding
	url     string
}

func (service *Service) nativeBlockingReason(ctx context.Context, job domain.ReviewJob, summary Summary, summaryURL string) (reviewrules.PromptData, error) {
	logger := gklog.L(ctx)
	var empty reviewrules.PromptData
	candidates := make([]nativeBlockingFinding, 0, len(summary.Metadata))
	for _, finding := range summary.Metadata {
		if finding.EffectiveSurface() == domain.FindingFile || finding.Validate() != nil {
			return empty, errors.New("native rejection requires an actionable metadata finding")
		}
		candidates = append(candidates, nativeBlockingFinding{finding: finding, url: summaryURL})
	}
	threads, err := service.github.ListReviewThreads(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.WarnContext(ctx, "Read native rejection findings failed", "err", err)
		return empty, fmt.Errorf("read native rejection findings: %w", err)
	}
	var footerData reviewrules.PromptData
	footer, err := renderPrompt(service.reviewPolicy, "inline.footer", footerData)
	if err != nil {
		return empty, err
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
			finding.Title = strings.TrimSpace(strings.TrimLeft(nativeHeading.FindString(thread.RootComment.Body), "# \t"))
			if finding.Title == "" {
				finding.Title = fmt.Sprintf("%s:%d", finding.Path, finding.EndLine)
			}
			finding.Importance = domain.MinimumFindingImportance
			if recorded, found := marker.FindFinding(thread.RootComment.Body); found {
				finding.Importance = recorded.Importance
			}
		}
		url := summaryURL
		if thread.RootComment.DatabaseID > 0 {
			url = threadCommentURL(thread, job.PullRequestRef)
		}
		candidates = append(candidates, nativeBlockingFinding{finding: finding, url: url})
	}
	var selected nativeBlockingFinding
	for _, candidate := range candidates {
		finding := candidate.finding
		if strings.TrimSpace(finding.Title) == "" || strings.TrimSpace(finding.Body) == "" {
			continue
		}
		if selected.finding.Body == "" || finding.Importance > selected.finding.Importance || finding.Importance == selected.finding.Importance && compareFindings(finding, selected.finding) < 0 {
			selected = candidate
		}
	}
	if selected.finding.Body == "" {
		return empty, errors.New("native rejection requires a readable blocking finding explanation")
	}
	label := strings.Join(strings.Fields(sanitizeReportText(selected.finding.Title)), " ")
	label = strings.ReplaceAll(label, "`", "")
	label = strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]").Replace(label)
	var data reviewrules.PromptData
	data.Input, data.SummaryURL = label, selected.url
	return data, nil
}

func nativeLegacyExplanation(body, footer string) string {
	body = strings.ReplaceAll(body, footer, "")
	body = replyLineBreaks.Replace(body)
	body = nativeHiddenMarkup.ReplaceAllString(body, "")
	body = nativeCodeBlock.ReplaceAllString(body, "")
	body = nativeHeading.ReplaceAllString(body, "")
	return nativeHTMLFormat.ReplaceAllString(body, "")
}

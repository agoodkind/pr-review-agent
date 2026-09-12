package review

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// recordOverview retains every answer from one chunk, including answers from
// halves created after a truncated response.
func (pass *chunkPass) recordOverview(chunk int, overview string) {
	overview = sanitizeReportProse(overview)
	if validateFullSentence("review overview", overview) != nil {
		return
	}
	pass.mu.Lock()
	defer pass.mu.Unlock()
	pass.overviewByChunk[chunk] = append(pass.overviewByChunk[chunk], overview)
}

// overviews returns chunk explanations in stable chunk and answer order.
func (pass *chunkPass) overviews() []string {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	chunks := make([]int, 0, len(pass.overviewByChunk))
	for chunk := range pass.overviewByChunk {
		chunks = append(chunks, chunk)
	}
	sort.Ints(chunks)
	ordered := make([]string, 0, len(pass.overviewByChunk))
	for _, chunk := range chunks {
		ordered = append(ordered, pass.overviewByChunk[chunk]...)
	}
	return ordered
}

// recordReportModel includes the final report call in the run's model and
// request accounting.
func (pass *chunkPass) recordReportModel(name string) {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	pass.models.add(name)
	pass.requests++
}

func (service *Service) publishCompletedReview(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
	pullRequest githubapp.PullRequest,
	head domain.HeadSHA,
	threads []githubapp.ReviewThread,
	summary Summary,
	state marker.State,
	progress *reviewProgress,
	pass *chunkPass,
) error {
	report, reportCalled := service.generateReport(
		ctx, pullRequest, pass.work.Files, threads, summary, pass,
	)
	summary.Report = report
	summary.Models = pass.analysis().Models
	if reportCalled {
		current, err := service.github.GetPullRequest(
			ctx, job.InstallationID, job.Repository, job.Number,
		)
		if err != nil {
			return service.failCheck(
				ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureRefresh, err,
			)
		}
		if current.Head != head {
			return service.cancelCheck(ctx, job, checkRun.ID)
		}
	}
	publicationCtx, cancelPublication := service.publicationContext(ctx)
	defer cancelPublication()
	return service.publishVerdict(publicationCtx, job, checkRun, summary, state, progress)
}

func (service *Service) updateVerdictBody(
	ctx context.Context,
	job domain.ReviewJob,
	standing githubapp.Review,
	body string,
) error {
	if standing.Body == body {
		return nil
	}
	logger := gklog.L(ctx)
	_, err := service.github.UpdateReview(
		ctx, job.InstallationID, job.Repository, job.Number, standing.ID, body,
	)
	if err != nil {
		logger.ErrorContext(ctx, "update verdict body", slog.String("err", err.Error()))
		return fmt.Errorf("update verdict body: %w", err)
	}
	return nil
}

func (service *Service) generateReport(
	ctx context.Context,
	pullRequest githubapp.PullRequest,
	files []diff.FileContext,
	threads []githubapp.ReviewThread,
	summary Summary,
	pass *chunkPass,
) (Report, bool) {
	fallback := fallbackReport(summary, pass.overviews())
	reporter, ok := service.model.(Reporter)
	if !ok {
		return fallback, false
	}
	reportCtx, cancel := context.WithTimeout(ctx, pass.settings.chunkTimeout)
	defer cancel()
	completion, err := reporter.Report(
		reportCtx,
		reportPrompt(pullRequest, files, threads, summary, pass.overviews(), service.botLogin),
	)
	if err != nil {
		gklog.L(ctx).ErrorContext(ctx, "write final review report", slog.String("err", err.Error()))
		return fallback, true
	}
	pass.recordReportModel(completion.Model)
	return completion.Report, true
}

func reportPrompt(
	pullRequest githubapp.PullRequest,
	files []diff.FileContext,
	threads []githubapp.ReviewThread,
	summary Summary,
	overviews []string,
	botLogin string,
) string {
	const instruction = "Write the final report for the single top-level review comment. " +
		"Use the supplied verdict exactly. Do not repeat actionable finding details because they are in inline review comments. " +
		"Explain the pull request's purpose, the important behavior changes, the current discussion state, and why the verdict follows.\n"
	var body strings.Builder
	fmt.Fprintf(
		&body,
		"Current title: %s\nCurrent description: %s\nVerdict: %s\nChanged files:",
		pullRequest.Title,
		pullRequest.Body,
		summary.Decision,
	)
	for _, file := range files {
		fmt.Fprintf(&body, "\n- %s (%s)", file.Path, file.Status)
	}
	body.WriteString("\n\nReviewed change overviews:")
	for _, overview := range overviews {
		body.WriteString("\n- ")
		body.WriteString(overview)
	}
	body.WriteString("\n\nActionable findings published as inline review comments:")
	for _, finding := range summary.Published {
		fmt.Fprintf(&body, "\n- %s:%d: %s", finding.Path, finding.EndLine, finding.Title)
	}
	if len(summary.Published) == 0 {
		body.WriteString(" none.")
	}
	body.WriteString("\n\nCurrent blocking locations:")
	for _, reason := range summary.Blocking {
		body.WriteString("\n- ")
		body.WriteString(reason)
	}
	if len(summary.Blocking) == 0 {
		body.WriteString(" none.")
	}
	body.WriteString("\n\nUnread changes:")
	for _, hunk := range summary.Omissions {
		body.WriteString("\n- ")
		body.WriteString(describeUnreadHunk(hunk))
	}
	if len(summary.Omissions) == 0 {
		body.WriteString(" none.")
	}
	if reason := sanitizeDecisionReason(summary.DecisionReason); reason != "" {
		body.WriteString("\nOmission decision explanation: ")
		body.WriteString(reason)
	}
	discussions := collectDisputes(threads, botLogin, summary.Head)
	if len(discussions.sections) > 0 {
		body.WriteString("\n\nCurrent inline discussions:\n")
		body.WriteString(strings.Join(discussions.sections, "\n\n"))
	}
	input := strings.ReplaceAll(body.String(), promptInputBegin, "<UNTRUSTED_INPUT>")
	input = strings.ReplaceAll(input, promptInputEnd, "<END_UNTRUSTED_INPUT>")
	maximumInput := config.MaximumPromptBytes - len(instruction) - len(promptInputBegin) -
		len(promptInputEnd) - 2
	return instruction + WrapUntrusted(truncateUTF8(input, maximumInput))
}

func fallbackReport(summary Summary, overviews []string) Report {
	walkthrough := make([]string, 0, len(overviews))
	for _, overview := range overviews {
		if value := sanitizeReportText(overview); value != "" {
			walkthrough = append(walkthrough, value)
		}
	}
	if len(walkthrough) == 0 {
		walkthrough = append(walkthrough, "The review examined the changed files and their current diff.")
	}
	reason := "The current pull request has no open actionable findings."
	if len(summary.Omissions) > 0 {
		if omissionReason := sanitizeDecisionReason(summary.DecisionReason); omissionReason != "" {
			reason = omissionReason
		}
	}
	if summary.Decision == domain.ReviewDecisionRequestChanges {
		if len(summary.Omissions) == 0 || reason == "" {
			reason = "The reasons listed below still require action."
		}
		if len(summary.Published) > 0 {
			reason += " The open inline review comments listed below still require action."
		}
		if len(summary.Fallback) > 0 {
			reason += " The findings below still require action."
		}
	}
	return Report{
		Summary:       "This review checked the current pull request against its stated purpose and current changes.",
		Walkthrough:   walkthrough,
		VerdictReason: reason,
	}
}

package review

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
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
	summary Summary,
	state marker.State,
	progress *reviewProgress,
	pass *chunkPass,
) error {
	report, reportCalled := service.generateReport(
		ctx, pullRequest, pass,
	)
	summary.Report = report
	summary.Models = pass.analysis().Models
	summary.Usage = UsageFromContext(ctx)
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
	return service.publishVerdict(publicationCtx, job, checkRun, summary, state)
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
	pass *chunkPass,
) (Report, bool) {
	fallback := SanitizeReport(fallbackReport(pass.overviews()))
	reporter, ok := service.model.(Reporter)
	if !ok {
		return fallback, false
	}
	reportCtx, cancel := context.WithTimeout(ctx, pass.settings.chunkTimeout)
	defer cancel()
	completion, err := reporter.Report(
		reportCtx,
		reportPrompt(pullRequest, pass.overviews()),
	)
	if err != nil {
		gklog.L(ctx).ErrorContext(ctx, "write final review report", slog.String("err", err.Error()))
		return fallback, true
	}
	pass.recordReportModel(completion.Model)
	report := SanitizeReport(completion.Report)
	if err := report.Validate(); err != nil {
		gklog.L(ctx).ErrorContext(ctx, "validate final review report", slog.String("err", err.Error()))
		return fallback, true
	}
	return report, true
}

func reportPrompt(
	pullRequest githubapp.PullRequest,
	overviews []string,
) string {
	const instruction = "Write the final report for the single top-level review comment. " +
		"Write at most two short summary sentences that state why the pull request exists and its resulting behavior. " +
		"Write at most four walkthrough items. Each item must state one distinct behavior that the summary does not already state. " +
		"Do not repeat the verdict, findings, coverage, omissions, file list, or inline discussions. " +
		"Use plain language before code names. Each sentence must name its subject and make sense without another sentence.\n"
	var body strings.Builder
	fmt.Fprintf(&body, "Current title: %s\nCurrent description: %s\n\nReviewed change overviews:", pullRequest.Title, pullRequest.Body)
	for _, overview := range overviews {
		body.WriteString("\n- ")
		body.WriteString(overview)
	}
	input := strings.ReplaceAll(body.String(), promptInputBegin, "<UNTRUSTED_INPUT>")
	input = strings.ReplaceAll(input, promptInputEnd, "<END_UNTRUSTED_INPUT>")
	maximumInput := config.MaximumPromptBytes - len(instruction) - len(promptInputBegin) -
		len(promptInputEnd) - 2
	return instruction + WrapUntrusted(truncateUTF8(input, maximumInput))
}

func fallbackReport(overviews []string) Report {
	walkthrough := make([]string, 0, len(overviews))
	for _, overview := range overviews {
		if value := sanitizeReportText(overview); value != "" {
			walkthrough = append(walkthrough, value)
		}
	}
	if len(walkthrough) == 0 {
		walkthrough = append(walkthrough, "The review examined the changed files and their current diff.")
	}
	return Report{
		Summary:     "This review checked the current pull request against its stated purpose and current changes.",
		Walkthrough: walkthrough,
	}
}

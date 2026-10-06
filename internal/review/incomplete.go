package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

func (service *Service) concludeIncomplete(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
	state marker.State,
	pass *chunkPass,
	summary Summary,
	progress *reviewProgress,
) error {
	logger := gklog.L(ctx)
	pending := len(state.Pending)
	unread := pass.unreadChunks()
	reason := chunkFailureReason(unread)
	statuses := providerStatuses(unread)
	classes := make([]string, 0)
	for _, class := range chunkFailureClasses(unread) {
		classes = append(classes, string(class))
	}
	recordAssessment(ctx, domain.AssessmentIncomplete, &summary, classes, quotaRecovery(statuses))
	cause := joinedChunkFailures(ctx, unread)
	dismissErr := service.dismissFailureVerdict(ctx, job, cause)
	detail := service.terminalFailureDetail(ctx, job, cause)
	addAttemptedModels(&summary, statuses)
	if _, err := persistAssessment(ctx, checkRun.ID); err != nil {
		return err
	}
	if err := service.upsertSummaryComment(ctx, job, summaryCommentContent{
		Prose: RenderIncompleteBody(summary, pending, reason, detail, statuses...) + reassessmentNotice(ctx),
		State: state,
	}); err != nil {
		return service.failCheck(
			ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureSummary, err,
		)
	}
	if err := service.completeCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		checkRun.ID,
		service.presentedConclusion(chunkFailureClasses(unread), checkConclusionDeclined),
		incompleteCheckTitle(pending, reason),
		incompleteCheckDetail(unread, summary, detail),
	); err != nil {
		logger.WarnContext(ctx, "complete incomplete review check", slog.String("err", err.Error()))
		return errors.Join(err, dismissErr)
	}
	markFailureReported(ctx)
	logger.InfoContext(
		ctx,
		"review job left chunks pending",
		slog.Int("pending", pending),
		slog.Int64("check_run_id", checkRun.ID),
	)
	return dismissErr
}

// incompleteCheckDetail is what the check run says about a run that could not
// read every chunk: which chunks went unread, the class of failure this service
// recognized, where the cause itself is, and the same progress table every
// other outcome reports.
//
// The provider's own sentence is not part of it. A check run is as public and
// as permanent as a pull request comment, and one live failure read "model
// provider returned HTTP 400 Bad Request: invalid_request_error:
// upstream_failed: upstream call failed: usage credits are exhausted", which is
// text this service never read and cannot unpublish.
func incompleteCheckDetail(failures []chunkFailure, summary Summary, detail string) string {
	parts := make([]string, 0, 4)
	if numbers := unreadChunkNumbers(failures); numbers != "" {
		parts = append(parts, "Chunks left unread: "+numbers+".")
	}
	if reason := chunkFailureReason(failures); reason != "" {
		parts = append(parts, reason)
	}
	if statuses := renderProviderStatuses(providerStatuses(failures)); statuses != "" {
		parts = append(parts, statuses)
	}
	parts = append(parts, detail, RenderDetails(summary))
	return strings.Join(parts, "\n\n")
}

// incompleteCheckTitle is the one line a reader sees before opening anything on
// a run that could not finish: how much went unread, and how to retry it.
func incompleteCheckTitle(pending int, reason string) string {
	if reason == checkFailureDailyBudget {
		return chunkCount(pending) + " could not be reviewed: app token limit exhausted."
	}
	if reason == checkFailureMixedUsage {
		return chunkCount(pending) + " could not be reviewed: app and provider usage exhausted."
	}
	return fmt.Sprintf("%s could not be reviewed. Apply `%s` to retry.",
		chunkCount(pending), domain.RerunReviewLabel)
}

package review

import (
	"context"
	"errors"
	"log/slog"
	"reflect"

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
	service.enrichProviderPeriodUsage(ctx, &summary)

	ctx, cancel := service.publicationContext(ctx)
	defer cancel()

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
	outcome, persistErr := persistAssessment(ctx, checkRun.ID)
	summary.Outcome = outcome
	var rendered renderState
	detail := rendered.capture(service.terminalFailureDetail(ctx, job, cause))

	addAttemptedModels(&summary, statuses)
	commentDetail := rendered.capture(incompleteCommentDetail(reason, detail))

	body := rendered.capture(
		RenderIncompleteBody(summary, pending, reason, commentDetail, statuses...),
	)
	title := rendered.capture(incompleteCheckTitle(pending, reason))
	checkDetail := rendered.capture(incompleteCheckDetail(unread, summary, detail))
	if rendered.err != nil {
		return errors.Join(cause, persistErr, rendered.err, dismissErr)
	}

	content := summaryCommentContent{Prose: body, State: state}
	summaryErr := service.upsertSummaryComment(ctx, job, content)
	if persistErr != nil {
		return &reportedFailureError{cause: errors.Join(cause, persistErr, summaryErr, dismissErr)}
	}

	if summaryErr != nil {
		failureErr := service.failCheck(
			ctx,
			job,
			checkRun.ID,
			progress.summary(service.now()),
			checkFailureSummary,
			summaryErr,
		)

		return errors.Join(dismissErr, failureErr)
	}

	if err := service.completeCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		checkRun.ID,
		service.presentedConclusion(chunkFailureClasses(unread), checkConclusionDeclined),
		title,
		checkDetail,
	); err != nil {
		logger.WarnContext(ctx, "complete incomplete review check", slog.String("err", err.Error()))
		return errors.Join(err, dismissErr)
	}

	logger.InfoContext(
		ctx,
		"review job left chunks pending",
		slog.Int("pending", pending),
		slog.Int64("check_run_id", checkRun.ID),
	)

	if dismissErr != nil {
		logger.WarnContext(
			ctx,
			"incomplete review withdrawal failed",
			slog.String("err", dismissErr.Error()),
		)

		return &reportedFailureError{cause: dismissErr}
	}

	return nil
}

func incompleteCommentDetail(reason, detail string) (string, error) {
	if reason == "" {
		return detail, nil
	}

	var rendered renderState

	return rendered.finish(
		rendered.capture(renderOutputParts(rendered.capture(renderOutputText(reason)), detail)),
	)
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
func incompleteCheckDetail(
	failures []chunkFailure,
	summary Summary,
	detail string,
) (string, error) {
	var state renderState

	return state.finish(
		state.capture(
			renderOutputTemplate(
				"output.incomplete-check-detail",
				reflect.ValueOf(
					struct {
						Numbers  string
						Reason   string
						Statuses string
						Detail   string
						Details  string
					}{
						unreadChunkNumbers(failures),
						state.capture(renderOutputText(chunkFailureReason(failures))),
						state.capture(renderProviderStatuses(providerStatuses(failures))),
						detail,
						state.capture(RenderDetails(summary)),
					},
				),
			),
		),
	)
}

// incompleteCheckTitle is the one line a reader sees before opening anything on
// a run that could not finish: how much went unread, and how to retry it.
func incompleteCheckTitle(pending int, reason string) (string, error) {
	var state renderState

	return state.finish(
		state.capture(
			renderOutputTemplate(
				"output.incomplete-check-title",
				reflect.ValueOf(
					struct {
						Chunks string
						Daily  bool
						Mixed  bool
						Label  string
					}{
						state.capture(chunkCount(pending)),
						reason == checkFailureDailyBudget,
						reason == checkFailureMixedUsage,
						domain.RerunReviewLabel,
					},
				),
			),
		),
	)
}

package review

// This file is what a run that could not read every chunk leaves behind.
//
// It is neither a success nor a failure, and it is not a judgment. A failure to
// read is not a finding: the run learned nothing about the code, so it has no
// grounds to request changes and no grounds to approve. It touches no review
// object at all. The two outputs that remain say what happened without ruling
// on the head: the one top level comment names what is still owed, and the
// check holds the merge gate without passing.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// concludeIncomplete ends a pass that could not read every chunk.
//
// It submits no verdict. An earlier design submitted a blocking review here, so
// a model provider outage turned every open pull request into a wall of
// requested changes that nobody had requested, and each looked exactly like a
// human reviewer objecting. The merge gate does not need the review: the check
// concludes without passing, so a required check holds the head anyway, and a
// repository that does not require the check has chosen not to gate on it.
//
// The check does not reach a passing conclusion. GitHub counts a required check
// concluded neutral as satisfying the gate, exactly as it counts one concluded
// skipped, so concluding either way would let a head with unread chunks merge
// on the strength of a run that admitted it had not finished. The title still
// separates "could not be reviewed" from "the review broke", so the reader
// learns which happened without the gate opening.
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
	if err := service.upsertSummaryComment(ctx, job, summaryCommentContent{
		Prose: RenderIncompleteBody(summary, pending, chunkFailureReason(unread), publicFailureDetail(job)),
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
		checkConclusionDeclined,
		incompleteCheckTitle(pending),
		incompleteCheckDetail(unread, summary, job),
	); err != nil {
		return err
	}
	logger.InfoContext(
		ctx,
		"review job left chunks pending",
		slog.Int("pending", pending),
		slog.Int64("check_run_id", checkRun.ID),
	)
	return nil
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
func incompleteCheckDetail(failures []chunkFailure, summary Summary, job domain.ReviewJob) string {
	parts := make([]string, 0, 4)
	if numbers := unreadChunkNumbers(failures); numbers != "" {
		parts = append(parts, "Chunks left unread: "+numbers+".")
	}
	if reason := chunkFailureReason(failures); reason != "" {
		parts = append(parts, reason)
	}
	parts = append(parts, publicFailureDetail(job), RenderDetails(summary))
	return strings.Join(parts, "\n\n")
}

// incompleteCheckTitle is the one line a reader sees before opening anything on
// a run that could not finish: how much went unread, and what happens next.
func incompleteCheckTitle(pending int) string {
	return fmt.Sprintf("%s could not be reviewed. The next push reviews %s.",
		chunkCount(pending), chunkPronoun(pending))
}

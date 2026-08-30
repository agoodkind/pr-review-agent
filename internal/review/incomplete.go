package review

// This file is what a run that could not read every chunk leaves behind.
//
// It is neither a success nor a failure. The run reached the end and knows what
// it read, so it publishes a verdict and a summary like any other; it just
// cannot claim the head. The three outputs say so consistently: a blocking
// review, a comment naming what is still owed, and a check that does not pass.

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
// It submits a blocking verdict. The run does not know what the chunks it never
// read contain, and withholding the verdict to avoid a wrong approval is what
// leaves the previous run's approval standing over a head nobody finished
// reading. The comment names what is left and that the next push reviews it.
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
	reviews []githubapp.Review,
	state marker.State,
	pass *chunkPass,
	summary Summary,
	progress *reviewProgress,
) error {
	logger := gklog.L(ctx)
	pending := len(state.Pending)
	unread := pass.unreadChunks()
	if err := service.publishPartialVerdict(ctx, job, reviews, summary, pending); err != nil {
		return service.failCheck(
			ctx, job, checkRun.ID, progress.summary(service.now()), checkFailurePublish, err,
		)
	}
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
		slog.String("decision", string(summary.Decision)),
		slog.Int64("check_run_id", checkRun.ID),
	)
	return nil
}

// publishPartialVerdict leaves exactly one blocking review saying this head
// could not be fully read.
//
// A run that finds the previous one rewrites it rather than submitting a
// second. Two incomplete runs in a row would otherwise leave two short blocking
// reviews, three would leave three, and a reader would have to work out which
// of them still describes the pull request.
//
// Rewriting keeps the block: an update changes a review's body and not its
// state, so the standing request for changes stays exactly as it was.
func (service *Service) publishPartialVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
	summary Summary,
	pending int,
) error {
	logger := gklog.L(ctx)
	body := RenderPartialVerdictBody(summary, pending)
	if existing, found := findPartialReview(reviews, service.botLogin); found {
		if _, err := service.github.UpdateReview(
			ctx,
			job.InstallationID,
			job.Repository,
			job.Number,
			existing.ID,
			body,
		); err != nil {
			logger.ErrorContext(ctx, "update partial verdict", slog.String("err", err.Error()))
			return fmt.Errorf("update partial verdict: %w", err)
		}
		logger.InfoContext(ctx, "partial verdict updated", slog.Int64("review_id", existing.ID))
		return nil
	}

	published, err := service.github.SubmitReview(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
		githubapp.SubmitReviewRequest{
			CommitID: summary.Head,
			Body:     body,
			Event:    summary.Decision,
			Comments: nil,
		},
	)
	if err != nil {
		logger.ErrorContext(ctx, "submit partial verdict", slog.String("err", err.Error()))
		return fmt.Errorf("submit partial verdict: %w", err)
	}
	logger.InfoContext(ctx, "partial verdict submitted", slog.Int64("review_id", published.ID))
	return nil
}

// findPartialReview returns the service's own standing partial verdict.
func findPartialReview(reviews []githubapp.Review, botLogin string) (githubapp.Review, bool) {
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		if marker.HasPartial(item.Body) {
			return item, true
		}
	}
	return githubapp.Review{ID: 0, CommitID: "", Author: "", Body: "", State: ""}, false
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

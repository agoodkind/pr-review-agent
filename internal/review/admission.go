package review

// Admission decides whether a delta is worth attempting at all. An oversized
// review is slow and shallow, so past the configured budget the service says
// it is skipping and why, rather than grinding to a timeout.
//
// Declining is not permission to merge. The check therefore stops short of any
// conclusion GitHub counts as passing, so an oversized and entirely unreviewed
// delta cannot merge on the strength of having been declined.

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// compareFileCap is the most files GitHub's compare endpoint will name in one
// range. Past it the response is silently short, so a delta that reaches the
// cap cannot be seen whole and must not be reviewed as though it had been.
const compareFileCap = 300

// admissionVerdict says whether to review the delta, and when not, why.
type admissionVerdict struct {
	Skip   bool
	Reason string
}

// admitDelta measures the delta against the configured budgets.
//
// The file budget can never exceed what one compare can name. Past that cap the
// range comes back short with nothing saying so, and reviewing it would report
// complete coverage over files nobody listed, which is the one thing this
// service must never do.
func admitDelta(fileCount int, chunkCount int, maxFiles int, maxChunks int) admissionVerdict {
	if maxFiles > compareFileCap {
		maxFiles = compareFileCap
	}
	if fileCount > maxFiles {
		return admissionVerdict{
			Skip:   true,
			Reason: fmt.Sprintf("%d files changed, over the %d file review budget", fileCount, maxFiles),
		}
	}
	if chunkCount > maxChunks {
		return admissionVerdict{
			Skip:   true,
			Reason: fmt.Sprintf("%d diff chunks, over the %d chunk review budget", chunkCount, maxChunks),
		}
	}
	return admissionVerdict{Skip: false, Reason: ""}
}

// deltaWork is the delta one run was admitted to review: the files it covers
// and the chunks the model will be asked about. Admission measures exactly
// these chunks, so nothing later re-derives a different set from the same diff.
type deltaWork struct {
	Files  []diff.FileContext
	Chunks []diff.Chunk
}

// collectAndAdmit gathers the diff and applies the admission gate before any
// model call. base is the durable state's last reviewed commit, or empty on
// first contact, so the collector reviews only the range since that commit
// instead of the whole pull request again. stop reports whether the caller
// must return err immediately: true with a nil err is a completed skip, true
// with a non-nil err is a completed failure, and false means the work is ready
// for the chunk loop.
func (service *Service) collectAndAdmit(
	ctx context.Context,
	job domain.ReviewJob,
	pullRequest githubapp.PullRequest,
	checkRun githubapp.CheckRun,
	base domain.HeadSHA,
	progress *reviewProgress,
	settings reviewSettings,
) (deltaWork, bool, error) {
	logger := gklog.L(ctx)
	empty := deltaWork{Files: nil, Chunks: nil}
	input, err := service.collector.CollectRange(ctx, job.PullRequestRef, pullRequest, base)
	if err != nil {
		logger.ErrorContext(ctx, "collect pull request diff", slog.String("err", err.Error()))
		return empty, true, service.failCheck(
			ctx, job, checkRun.ID, progress.summary(service.now()),
			checkFailureDiff, fmt.Errorf("collect pull request diff: %w", err),
		)
	}
	progress.reached("the diff")

	chunks, err := diff.ChunkInput(input, config.MaximumPromptBytes)
	if err != nil {
		logger.ErrorContext(ctx, "chunk input", slog.String("err", err.Error()))
		return empty, true, service.failCheck(
			ctx, job, checkRun.ID, progress.summary(service.now()),
			checkFailureDiff, fmt.Errorf("chunk input: %w", err),
		)
	}
	work := deltaWork{Files: input.Files, Chunks: chunks}

	verdict := admitDelta(len(input.Files), len(chunks), settings.maxFiles, settings.maxChunks)
	if !verdict.Skip {
		return work, false, nil
	}
	logger.InfoContext(
		ctx,
		"review job skipped",
		slog.String("reason", verdict.Reason),
		slog.Int("files", len(input.Files)),
		slog.Int("chunks", len(chunks)),
	)
	if err := service.declineReview(ctx, job, checkRun.ID, verdict); err != nil {
		// A skip that cannot be written is still a run that has to end. Without
		// this the error travels back to the dispatcher, which only logs it, and
		// the check stays in progress with nothing on the pull request saying why.
		return empty, true, service.failCheck(
			ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureSkip, err,
		)
	}
	// The caller stops on a skip and never reads the work, so what was measured
	// travels back unchanged rather than as a second zero value.
	return work, true, nil
}

// declineReview leaves a delta the admission gate refused exactly as it found
// it. The summary comment says why, the check completes without passing, and no
// review object is touched, because a delta the service cannot review well is
// not a review failure and must never stand as one.
//
// The conclusion is action_required rather than skipped. GitHub counts a
// required check concluded skipped as passing, so concluding that way would let
// an entirely unreviewed oversized delta merge with any earlier approval still
// standing, which is the opposite of what admission is for. action_required
// holds the gate and states the truth of the situation: a person has to split
// the pull request, raise its budget, or ask for the review. The title and the
// summary still say the review was skipped, so a reader can tell "too large to
// review" from "the review broke".
func (service *Service) declineReview(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	verdict admissionVerdict,
) error {
	logger := gklog.L(ctx)
	// The state is read and written in one call, so nothing this run keeps has
	// to be carried in from an earlier read that a concurrent edit could have
	// overtaken. It is the same reason the failure notice is written this way.
	if err := service.upsertSummaryCommentFrom(ctx, job, func(existing marker.State) summaryCommentContent {
		return summaryCommentContent{
			Prose: RenderSkipBody(verdict.Reason),
			State: marker.State{
				// The baseline stays where it was. Advancing it to a head nobody
				// read would let the next small push measure only its own
				// commits, approve, and merge the declined range as though it
				// had been reviewed, and would let a plain redelivery of the
				// same commit return "already reviewed" with no push at all.
				// The oversized range therefore stays in every later delta and
				// keeps being declined until a person splits the pull request,
				// raises its budget, or asks for the review.
				LastReviewed: existing.LastReviewed,
				RunID:        job.DeliveryID,
				Status:       marker.StateSkipped,
				// A declined delta read no chunk, so it has nothing to add to
				// the work already recorded and no business taking any away.
				// Erasing the chunks an earlier run finished would send the
				// next run that is allowed to review over work this pull
				// request has already paid for.
				Pending:   existing.Pending,
				Completed: existing.Completed,
				// The forcing delivery carries forward for the same reason. A
				// decline is not that delivery finishing, so dropping its record
				// here would send its own resume over the whole pull request
				// again.
				ForcedBy: existing.ForcedBy,
				// So does the recorded shortfall. A decline read nothing, so it
				// cannot have cleared a hunk an earlier run could not read.
				Unread: existing.Unread,
			},
		}
	}); err != nil {
		logger.ErrorContext(ctx, "upsert skip notice", slog.String("err", err.Error()))
		return fmt.Errorf("upsert skip notice: %w", err)
	}
	if err := service.completeCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		checkRunID,
		checkConclusionDeclined,
		checkSummarySkipped,
		verdict.Reason,
	); err != nil {
		logger.ErrorContext(ctx, "complete skipped check run", slog.String("err", err.Error()))
		return fmt.Errorf("complete skipped check run: %w", err)
	}
	logger.InfoContext(ctx, "review job skip completed", slog.Int64("check_run_id", checkRunID))
	return nil
}

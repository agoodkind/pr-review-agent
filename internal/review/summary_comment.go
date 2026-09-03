package review

// This file keeps the one top level comment the service owns.
//
// The comment is the service's memory and its face. The state marker inside
// it is what a later invocation resumes from, and the prose above the marker
// is what a person reads. Splitting those across objects is how the summary,
// the verdict, and the head drifted apart in production.

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// summaryCommentContent is what one run wants the comment to say: the prose a
// person reads, and the durable state the next run resumes from.
type summaryCommentContent struct {
	Prose string
	State marker.State
}

// upsertSummaryComment writes the prose and the state marker into the
// service's one top level comment, creating it only when this pull request
// has none.
func (service *Service) upsertSummaryComment(
	ctx context.Context,
	job domain.ReviewJob,
	content summaryCommentContent,
) error {
	return service.upsertSummaryCommentFrom(ctx, job, func(marker.State) summaryCommentContent {
		return content
	})
}

// announceStart says the review has begun, in the one comment every later stage
// rewrites.
//
// Until this existed the pull request said nothing until the first chunk came
// back, which on a large delta is minutes of a pending check with no way to tell
// a slow review from one that never began. The durable state is carried forward
// untouched: nothing has been reviewed yet, so nothing about the checkpoint may
// change here.
//
// A failure to post is logged and swallowed. Not being able to say a review
// started is no reason to refuse to run it, and every later stage writes the
// same comment again.
func (service *Service) announceStart(ctx context.Context, job domain.ReviewJob, head domain.HeadSHA) {
	err := service.upsertSummaryCommentFrom(ctx, job, func(state marker.State) summaryCommentContent {
		return summaryCommentContent{Prose: RenderStartedBody(head), State: state}
	})
	if err != nil {
		gklog.L(ctx).WarnContext(ctx, "announce review start", slog.String("err", err.Error()))
	}
}

// upsertSummaryCommentFrom writes content built from the state the comment
// already carries, in the same read the write uses.
//
// A run that must keep part of that state, such as a failed run keeping the
// commit the last completed run reviewed, would otherwise read the comment
// once to learn it and again to write it, and could lose a concurrent edit
// between the two.
//
// The state marker is appended here rather than by each caller, because the
// marker is the only way the next run finds this comment. A body written
// without one makes that run miss the comment and open a second, and the write
// most likely to forget it is a failure notice, at the moment the pull request
// is already in trouble.
func (service *Service) upsertSummaryCommentFrom(
	ctx context.Context,
	job domain.ReviewJob,
	build func(marker.State) summaryCommentContent,
) error {
	logger := gklog.L(ctx)
	comments, err := service.github.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "list issue comments", slog.String("err", err.Error()))
		return fmt.Errorf("list issue comments: %w", err)
	}
	if existing, found := findSummaryComment(comments, service.botLogin); found {
		existingState, _ := marker.DecodeState(existing.Body)
		if _, err := service.github.UpdateIssueComment(
			ctx,
			job.InstallationID,
			job.Repository,
			existing.ID,
			renderSummaryComment(build(existingState)),
		); err != nil {
			logger.ErrorContext(ctx, "update summary comment", slog.String("err", err.Error()))
			return fmt.Errorf("update summary comment: %w", err)
		}
		logger.InfoContext(ctx, "summary comment updated", slog.Int64("comment_id", existing.ID))
		return nil
	}
	firstContent := build(marker.State{LastReviewed: "", RunID: "", Status: "", Pending: nil, Completed: nil, ForcedBy: ""})
	created, err := service.github.CreateIssueComment(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
		renderSummaryComment(firstContent),
	)
	if err != nil {
		logger.ErrorContext(ctx, "create summary comment", slog.String("err", err.Error()))
		return fmt.Errorf("create summary comment: %w", err)
	}
	logger.InfoContext(ctx, "summary comment created", slog.Int64("comment_id", created.ID))
	return nil
}

// renderSummaryComment puts the state marker under the prose. Every write to
// the comment goes through here, so no body can reach GitHub without it.
func renderSummaryComment(content summaryCommentContent) string {
	return content.Prose + "\n" + marker.EncodeState(content.State)
}

// findSummaryComment returns the bot's own comment carrying the state
// marker, the same way findSummaryReview finds the bot's visible review.
func findSummaryComment(comments []githubapp.IssueComment, botLogin string) (githubapp.IssueComment, bool) {
	for _, comment := range comments {
		if comment.Author != botLogin {
			continue
		}
		if marker.HasState(comment.Body) {
			return comment, true
		}
	}
	return githubapp.IssueComment{ID: 0, Author: "", Body: ""}, false
}

// readState returns the durable state from the service's comment, and false
// when this pull request has never been reviewed.
func (service *Service) readState(
	ctx context.Context,
	job domain.ReviewJob,
) (marker.State, bool, error) {
	logger := gklog.L(ctx)
	comments, err := service.github.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "list issue comments", slog.String("err", err.Error()))
		return marker.State{LastReviewed: "", RunID: "", Status: "", Pending: nil, Completed: nil, ForcedBy: ""}, false, fmt.Errorf("list issue comments: %w", err)
	}
	if comment, found := findSummaryComment(comments, service.botLogin); found {
		if state, ok := marker.DecodeState(comment.Body); ok {
			return state, true, nil
		}
	}
	return marker.State{LastReviewed: "", RunID: "", Status: "", Pending: nil, Completed: nil, ForcedBy: ""}, false, nil
}

// loadDurableState reads the run's durable state for the dedup log line and
// for computing the delta base, logging rather than failing the run when the
// read itself errors: a lost read costs a redundant full review, not a wrong
// one, so the run still proceeds.
func (service *Service) loadDurableState(ctx context.Context, job domain.ReviewJob) (marker.State, bool) {
	logger := gklog.L(ctx)
	state, hasState, err := service.readState(ctx, job)
	if err != nil {
		logger.WarnContext(ctx, "read durable review state", slog.String("err", err.Error()))
		return marker.State{LastReviewed: "", RunID: "", Status: "", Pending: nil, Completed: nil, ForcedBy: ""}, false
	}
	if hasState {
		logger.InfoContext(
			ctx,
			"durable review state loaded",
			slog.String("status", state.Status),
			slog.String("run_id", state.RunID),
		)
	}
	return state, hasState
}

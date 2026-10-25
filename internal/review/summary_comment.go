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
	"strings"

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
	return service.upsertSummaryCommentFrom(ctx, job, func(marker.State, string) summaryCommentContent {
		return content
	})
}

// announceStart records the current run without moving its checkpoint.
func (service *Service) announceStart(ctx context.Context, job domain.ReviewJob, head domain.HeadSHA) {
	err := service.upsertSummaryCommentFrom(ctx, job, func(state marker.State, existingBody string) summaryCommentContent {
		state.RunID = job.DeliveryID
		state.Status = marker.StateReviewing
		prose := RenderStartedBody(head)
		if preserveCheckpointSummary(state, existingBody) {
			prose = renderCheckpointNotice(existingBody, strings.TrimPrefix(prose, "## Review\n\n"))
		}
		return summaryCommentContent{Prose: prose, State: state}
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
	build func(marker.State, string) summaryCommentContent,
) error {
	logger := gklog.L(ctx)
	comments, err := service.github.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "list issue comments", slog.String("err", err.Error()))
		return fmt.Errorf("list issue comments: %w", err)
	}
	if existing, found := findSummaryComment(comments, service.botLogin); found {
		existingState, decoded := marker.DecodeState(existing.Body)
		if marker.HasState(existing.Body) && !decoded {
			return fmt.Errorf("the saved review state cannot be decoded")
		}
		if !decoded {
			existingState.RunID = job.DeliveryID
			existingState.Status = marker.StateDone
		}
		if _, err := service.github.UpdateIssueComment(
			ctx,
			job.InstallationID,
			job.Repository,
			existing.ID,
			renderSummaryComment(build(existingState, existing.Body)),
		); err != nil {
			logger.ErrorContext(ctx, "update summary comment", slog.String("err", err.Error()))
			return fmt.Errorf("update summary comment: %w", err)
		}
		logger.InfoContext(ctx, "summary comment updated", slog.Int64("comment_id", existing.ID))
		return nil
	}
	firstContent := build(
		marker.State{
			PolicyRevision: "",
			LastReviewed:   "", RunID: job.DeliveryID, Status: marker.StateDone, Pending: nil,
			Completed: nil, ForcedBy: "", Unread: nil,
		},
		"",
	)
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
		if marker.HasState(comment.Body) || marker.HasSummary(comment.Body) {
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
		return marker.State{PolicyRevision: "", LastReviewed: "", RunID: "", Status: "", Pending: nil, Completed: nil, ForcedBy: "", Unread: nil}, false, fmt.Errorf("list issue comments: %w", err)
	}
	if comment, found := findSummaryComment(comments, service.botLogin); found {
		if state, ok := marker.DecodeState(comment.Body); ok {
			return state, true, nil
		}
		if marker.HasState(comment.Body) {
			return marker.State{}, false, fmt.Errorf("the saved review state cannot be decoded")
		}
	}
	return marker.State{PolicyRevision: "", LastReviewed: "", RunID: "", Status: "", Pending: nil, Completed: nil, ForcedBy: "", Unread: nil}, false, nil
}

// loadDurableState requires a successful read before the run selects unfinished work.
func (service *Service) loadDurableState(ctx context.Context, job domain.ReviewJob) (marker.State, bool, error) {
	logger := gklog.L(ctx)
	state, hasState, err := service.readState(ctx, job)
	if err != nil {
		logger.WarnContext(ctx, "read durable review state", slog.String("err", err.Error()))
		return state, hasState, fmt.Errorf("read durable review state: %w", err)
	}
	if hasState {
		logger.InfoContext(
			ctx,
			"durable review state loaded",
			slog.String("status", state.Status),
			slog.String("run_id", state.RunID),
		)
	}
	return state, hasState, nil
}

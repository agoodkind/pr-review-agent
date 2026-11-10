package review

import (
	"context"
	"fmt"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

func validNativeMetadataBlock(native githubapp.Review) bool {
	record, found, err := decodeMetadataRecord(native.Body)
	return err == nil && found && record.Head == native.CommitID && len(record.Findings) > 0
}

func (service *Service) savedMetadataStillBlocks(
	ctx context.Context,
	job domain.ReviewJob,
	comments []githubapp.IssueComment,
) (bool, error) {
	comment, found := findSummaryComment(comments, service.botLogin)
	if !found {
		return true, nil
	}

	record, recorded, err := decodeMetadataRecord(comment.Body)
	if err != nil {
		logger := gklog.L(ctx)
		logger.WarnContext(ctx, "read metadata before failure dismissal", "err", err)

		return true, fmt.Errorf("read metadata before failure dismissal: %w", err)
	}

	if !recorded || record.Head != job.Head || len(record.Findings) > 0 {
		return true, nil
	}

	state, valid := marker.DecodeState(comment.Body)

	head, completed := marker.FindReview(comment.Body)
	if !valid || !completed || head != job.Head || !completedHeadState(state, job.Head) ||
		(state.Status != marker.StateDone && state.Status != marker.StateFailed) {
		return true, nil
	}

	current, err := service.github.GetPullRequest(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
	)
	if err != nil {
		logger := gklog.L(ctx)
		logger.WarnContext(ctx, "verify resolved metadata before failure dismissal", "err", err)

		return true, fmt.Errorf("verify resolved metadata before failure dismissal: %w", err)
	}

	return current.Head != job.Head || record.Revision != diff.PullRequestRevision(current), nil
}

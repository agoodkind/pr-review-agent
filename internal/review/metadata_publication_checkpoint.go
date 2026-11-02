package review

import (
	"context"
	"errors"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

func (service *Service) checkpointRefreshedMetadata(ctx context.Context, job domain.ReviewJob, summary Summary) error {
	record := metadataRecordFromSummary(summary)
	encoded := encodeMetadataRecord(record)
	if encoded == "" {
		return errors.New("cannot persist the current metadata assessment")
	}
	return service.upsertSummaryCommentFromChecked(ctx, job, func(state marker.State, body string) (summaryCommentContent, error) {
		prose := stripDurableStateMarker(body)
		if start := strings.LastIndex(prose, metadataRecordPrefix); start >= 0 {
			end := strings.Index(prose[start:], " -->")
			if end < 0 {
				return summaryCommentContent{}, errors.New("cannot replace an incomplete metadata review state")
			}
			prose = prose[:start] + encoded + prose[start+end+len(" -->"):]
		} else {
			prose += "\n\n" + encoded
		}
		if state.Status != marker.StateReviewing {
			state.Status = marker.StateFailed
		}
		return summaryCommentContent{Prose: prose, State: state}, nil
	})
}

func (service *Service) checkpointCompletedPublication(ctx context.Context, job domain.ReviewJob, checkRunID int64, summary Summary, state marker.State) error {
	if !completedHeadState(state, job.Head) {
		return nil
	}
	pendingPublication := state
	pendingPublication.Status = marker.StateFailed
	if err := service.upsertSummaryComment(ctx, job, summaryCommentContent{Prose: RenderBody(summary), State: pendingPublication}); err != nil {
		return service.failCheck(ctx, job, checkRunID, summary, checkFailureSummary, err)
	}
	if _, err := service.publicationPullRequest(ctx, job, summary.MetadataRevision); err != nil {
		return service.failCheck(ctx, job, checkRunID, summary, checkFailureRefresh, err)
	}
	return nil
}

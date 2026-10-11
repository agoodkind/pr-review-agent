package review

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

type assessmentSnapshot struct {
	metadata         metadataReviewRecord
	metadataRecorded bool
	// Refresh reports malformed metadata after the reviewing-state and head guards.
	metadataError     error
	omissions         []unreadHunk
	omissionsAccepted bool
	decisionReason    string
	approvalWithheld  bool
	legacyWholeHead   bool
}

func parseAssessmentSnapshot(body string) assessmentSnapshot {
	metadata, recorded, err := decodeMetadataRecord(body)
	snapshot := assessmentSnapshot{
		metadata:          metadata,
		metadataRecorded:  recorded,
		metadataError:     err,
		omissions:         decodeOmissionMarker(body),
		omissionsAccepted: false,
		decisionReason:    decodeDecisionReasonMarker(body),
		approvalWithheld:  strings.Contains(body, approvalWithheldMarker),
		legacyWholeHead:   !strings.Contains(body, unreviewedHeadReason),
	}
	if accepted, found := decodeOmissionDecisionMarker(body); found {
		snapshot.omissionsAccepted = accepted
	}
	return snapshot
}

// A completed summary records the assessment independently of native approvals.
func (service *Service) completedSummaryAssessment(
	ctx context.Context,
	job domain.ReviewJob,
) (assessmentSnapshot, bool, error) {
	logger := gklog.L(ctx)
	var empty assessmentSnapshot
	comments, err := service.github.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "read completed summary assessment", slog.String("err", err.Error()))
		return empty, false, fmt.Errorf("read completed summary assessment: %w", err)
	}
	comment, found := findSummaryComment(comments, service.botLogin)
	head, marked := marker.FindReview(comment.Body)
	if !found || !marked || head != job.Head {
		return empty, false, nil
	}
	state, hasState := marker.DecodeState(comment.Body)
	completed := hasState && state.Status == marker.StateDone && state.LastReviewed == job.Head && len(state.Pending) == 0
	snapshot := parseAssessmentSnapshot(comment.Body)
	if !completed && !snapshot.approvalWithheld {
		return empty, false, nil
	}
	return snapshot, true, nil
}

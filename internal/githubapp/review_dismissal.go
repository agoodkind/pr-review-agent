package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/domain"
)

// ReviewDismissal recovers the original decision after GitHub changes the review state.
type ReviewDismissal struct {
	OriginalDecision domain.ReviewDecision
	ReviewID         int64
	Actor            string
	Message          string
}

type reviewDismissalEvent struct {
	Event string `json:"event"`
	Actor struct {
		Login string `json:"login"`
	} `json:"actor"`
	Review *struct {
		ID      json.Number          `json:"review_id"`
		Message *string              `json:"dismissal_message"`
		State   dismissedReviewState `json:"state"`
	} `json:"dismissed_review"`
}

// FindReviewDismissal reads every timeline page before returning withdrawal evidence.
func (client *Client) FindReviewDismissal(ctx context.Context, installationID int64, repo domain.Repository, number int, reviewID int64) (ReviewDismissal, bool, error) {
	result := ReviewDismissal{OriginalDecision: "", ReviewID: 0, Actor: "", Message: ""}
	found := false
	path := client.repoPath(repo, fmt.Sprintf("/issues/%d/timeline", number))
	err := client.doRESTPaginated(ctx, installationID, path, func(page []byte) (int, error) {
		trimmed := bytes.TrimSpace(page)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			return 0, errors.New("review dismissal timeline is not an array")
		}
		var events []reviewDismissalEvent
		if err := json.Unmarshal(page, &events); err != nil {
			client.logger.WarnContext(ctx, "decode review dismissal timeline", slog.String("err", err.Error()))
			return 0, errors.New("decode review dismissal timeline")
		}
		for _, event := range events {
			if event.Event == "" {
				return 0, errors.New("review dismissal timeline event is incomplete")
			}
			if event.Event != "review_dismissed" {
				continue
			}
			if event.Review == nil || event.Actor.Login == "" || event.Review.Message == nil {
				return 0, errors.New("review dismissal timeline event is incomplete")
			}
			id, err := event.Review.ID.Int64()
			if err != nil {
				client.logger.WarnContext(ctx, "decode dismissed review ID", slog.String("err", err.Error()))
				return 0, errors.New("decode dismissed review ID")
			}
			if id == reviewID {
				decision, err := originalDismissedDecision(event.Review.State)
				if err != nil {
					client.logger.WarnContext(ctx, "decode original dismissed decision", slog.String("err", err.Error()))
					return 0, err
				}
				result = ReviewDismissal{OriginalDecision: decision, ReviewID: id, Actor: event.Actor.Login, Message: *event.Review.Message}
				found = true
			}
		}
		return len(events), nil
	})
	if err != nil {
		return result, false, err
	}
	return result, found, nil
}

type dismissedReviewState string

const (
	dismissedStateApproved  dismissedReviewState = "approved"
	dismissedStateRequested dismissedReviewState = "changes_requested"
	dismissedStateCommented dismissedReviewState = "commented"
	dismissedStateUnknown   dismissedReviewState = ""
)

func originalDismissedDecision(state dismissedReviewState) (domain.ReviewDecision, error) {
	switch state {
	case dismissedStateApproved:
		return domain.ReviewDecisionApprove, nil
	case dismissedStateRequested:
		return domain.ReviewDecisionRequestChanges, nil
	case dismissedStateCommented:
		return domain.ReviewDecisionComment, nil
	case dismissedStateUnknown:
		return "", nil
	default:
		return "", errors.New("the dismissed review has an invalid original state")
	}
}

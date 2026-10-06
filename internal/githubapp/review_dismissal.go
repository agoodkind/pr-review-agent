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

// ReviewDismissal records the actor and message from the matching timeline event.
type ReviewDismissal struct {
	ReviewID int64
	Actor    string
	Message  string
}

type reviewDismissalEvent struct {
	Event string `json:"event"`
	Actor struct {
		Login string `json:"login"`
	} `json:"actor"`
	Review *struct {
		ID      json.Number `json:"review_id"`
		Message *string     `json:"dismissal_message"`
	} `json:"dismissed_review"`
}

// FindReviewDismissal reads every timeline page before returning withdrawal evidence.
func (client *Client) FindReviewDismissal(ctx context.Context, installationID int64, repo domain.Repository, number int, reviewID int64) (ReviewDismissal, bool, error) {
	result := ReviewDismissal{ReviewID: 0, Actor: "", Message: ""}
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
				result = ReviewDismissal{ReviewID: id, Actor: event.Actor.Login, Message: *event.Review.Message}
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

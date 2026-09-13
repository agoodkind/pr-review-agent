package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/domain"
)

type dismissReviewBody struct {
	Message string `json:"message"`
}

// DismissReview withdraws one submitted review without changing its comments.
func (client *Client) DismissReview(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
	reviewID int64,
	message string,
) (Review, error) {
	path := client.repoPath(repo, fmt.Sprintf("/pulls/%d/reviews/%d/dismissals", number, reviewID))
	encoded, err := json.Marshal(dismissReviewBody{Message: message})
	if err != nil {
		client.logger.ErrorContext(ctx, "marshal review dismissal", slog.String("err", err.Error()))
		return Review{}, errors.New("marshal review dismissal")
	}
	body, err := client.doREST(ctx, installationID, "PUT", path, nil, encoded)
	if err != nil {
		return Review{}, err
	}
	return decodeReview(body, "dismissed", ctx, client)
}

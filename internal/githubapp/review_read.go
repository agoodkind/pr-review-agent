package githubapp

import (
	"context"
	"fmt"

	"goodkind.io/pr-review-agent/internal/domain"
)

// GetReview performs the separate API read needed to detect concurrent body edits.
func (client *Client) GetReview(ctx context.Context, installationID int64, repo domain.Repository, number int, reviewID int64) (Review, error) {
	path := client.repoPath(repo, fmt.Sprintf("/pulls/%d/reviews/%d", number, reviewID))
	body, err := client.doREST(ctx, installationID, "GET", path, nil, nil)
	if err != nil {
		return Review{}, err
	}
	return decodeReview(body, "read", ctx, client)
}

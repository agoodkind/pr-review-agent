package githubapp

import (
	"context"
	"errors"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

// SearchRepository finds literal matches in the private reviewed-head snapshot.
func (client *Client) SearchRepository(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	query string,
) ([]string, error) {
	if strings.TrimSpace(query) == "" || strings.ContainsAny(query, "\r\n\x00") {
		return nil, errors.New("repository search requires a nonempty literal query")
	}

	scope, err := client.repositorySnapshotScope(ctx, installationID, repo)
	if err != nil {
		return nil, err
	}

	if scope == nil {
		return nil, errors.New("repository search requires an active repository snapshot scope")
	}

	return scope.search(ctx, query)
}

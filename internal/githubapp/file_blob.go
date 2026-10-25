package githubapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"goodkind.io/pr-review-agent/internal/domain"
)

const githubRawBlobAcceptHeader = "application/vnd.github.raw+json"

// getFileBlob uses the immutable blob from the pinned Contents API response.
func (client *Client) getFileBlob(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	file fileContentResponse,
) ([]byte, error) {
	sha, err := domain.ParseHeadSHA(file.SHA)
	if err != nil {
		return nil, errors.New("large file content requires a valid blob SHA")
	}
	if file.Size == nil || *file.Size < 0 {
		return nil, errors.New("large file content requires a nonnegative byte size")
	}
	if *file.Size > maxResponseBytes {
		return nil, fmt.Errorf("file content requires %d bytes; the GitHub response limit is %d bytes", *file.Size, maxResponseBytes)
	}
	path := client.repoPath(repo, "/git/blobs/"+string(sha))
	content, err := client.doRESTWithAccept(ctx, installationID, http.MethodGet, path, nil, nil, githubRawBlobAcceptHeader)
	if err != nil {
		client.logger.ErrorContext(ctx, "load pinned file blob", slog.Any("err", err))
		return nil, fmt.Errorf("load pinned file blob: %w", err)
	}
	if len(content) != *file.Size {
		return nil, fmt.Errorf("file blob contains %d bytes; expected %d bytes", len(content), *file.Size)
	}
	return content, nil
}

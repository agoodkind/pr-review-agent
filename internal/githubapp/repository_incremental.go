package githubapp

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"goodkind.io/pr-review-agent/internal/domain"
)

// IncrementalHistoryUnavailableError identifies a previous revision absent
// from the current pull request's acquired Git history.
//
// Current-history acquisition errors retain their original causes.
//
// Example:
//
//	var unavailable *IncrementalHistoryUnavailableError
//	errors.As(err, &unavailable)
type IncrementalHistoryUnavailableError struct {
	Revision domain.HeadSHA
}

// Error identifies the previous revision that requires a full review.
//
// The collector recognizes this condition independently of HTTP statuses.
//
// Example:
//
//	message := unavailable.Error()
func (unavailable *IncrementalHistoryUnavailableError) Error() string {
	return fmt.Sprintf("previous review revision %s is unavailable in current Git history", unavailable.Revision)
}

// CompareIncremental acquires the current pull request's legitimate history
// before comparing its head with a locally available previous revision.
//
// The caller supplies metadata it already read for the current review.
// An unavailable previous revision does not cause a Git request for that SHA.
//
// Example:
//
//	comparison, err := client.CompareIncremental(ctx, installationID, repo, previous, current)
func (client *Client) CompareIncremental(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	previous domain.HeadSHA,
	current PullRequest,
) (Comparison, error) {
	_, scopeError := client.repositorySnapshotScope(ctx, installationID, repo)
	if scopeError != nil {
		return Comparison{}, scopeError
	}

	return client.compareRepositoryCommits(ctx, installationID, repo, current.Base, current.Head, previous)
}

func (client *Client) incrementalRepositoryBase(
	ctx context.Context,
	key repositoryCacheKey,
	entry *cachedRepository,
	previous domain.HeadSHA,
	head plumbing.Hash,
) (*object.Commit, error) {
	contextError := ctx.Err()
	if contextError != nil {
		return nil, repositoryAccessError("read previous Git revision", contextError)
	}

	hash, err := immutableGitHash(previous)
	if err != nil {
		return nil, err
	}

	_, err = object.GetCommit(entry.repository, hash)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		client.logger.WarnContext(ctx, "Previous revision is absent from current Git history", "revision", previous)

		return nil, &IncrementalHistoryUnavailableError{Revision: previous}
	}

	if err != nil {
		return nil, repositoryAccessError("read previous Git revision", err)
	}

	return client.repositoryMergeBase(ctx, key, entry, hash, head)
}

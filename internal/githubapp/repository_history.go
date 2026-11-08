package githubapp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func (client *Client) selectRepositoryMergeBase(
	ctx context.Context,
	key repositoryCacheKey,
	entry *cachedRepository,
	base plumbing.Hash,
	head plumbing.Hash,
) (*object.Commit, error) {
	roots := []plumbing.Hash{base, head}
	depth := initialAncestryDepth

	for {
		missing, err := missingCompleteAncestors(ctx, entry, roots)
		if err != nil {
			return nil, err
		}

		if len(missing) == 0 {
			break
		}

		err = client.fetchRepositoryAncestry(ctx, key, entry, missing, depth)
		if err != nil {
			return nil, err
		}

		depth = min(depth*ancestryDepthMultiplier, maximumSnapshotEntries)
	}

	baseCommit, err := object.GetCommit(entry.repository, base)
	if err != nil {
		return nil, repositoryAccessError("read comparison base commit", err)
	}

	headCommit, err := object.GetCommit(entry.repository, head)
	if err != nil {
		return nil, repositoryAccessError("read comparison head commit", err)
	}

	bestBases, err := baseCommit.MergeBase(headCommit)
	if err != nil {
		return nil, repositoryAccessError("compute Git merge bases", err)
	}

	if len(bestBases) == 0 {
		return nil, newAPIError(http.StatusUnprocessableEntity, "Git revisions do not share an ancestor")
	}

	slices.SortFunc(bestBases, func(left *object.Commit, right *object.Commit) int {
		return bytes.Compare(left.Hash[:], right.Hash[:])
	})

	return bestBases[0], nil
}

func missingCompleteAncestors(
	ctx context.Context,
	entry *cachedRepository,
	roots []plumbing.Hash,
) ([]plumbing.Hash, error) {
	pending := slices.Clone(roots)
	visited := make(map[plumbing.Hash]bool)
	missing := make([]plumbing.Hash, 0)

	for len(pending) > 0 {
		contextError := ctx.Err()
		if contextError != nil {
			return nil, repositoryAccessError("read Git ancestry", contextError)
		}

		hash := pending[0]
		pending = pending[1:]

		if visited[hash] {
			continue
		}

		visited[hash] = true
		if len(visited) > maximumSnapshotEntries {
			return nil, errRepositoryAncestryLimit
		}

		commit, err := object.GetCommit(entry.repository, hash)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			missing = append(missing, hash)

			continue
		}

		if err != nil {
			return nil, repositoryAccessError("read Git ancestry", err)
		}

		pending = append(pending, commit.ParentHashes...)
	}

	return missing, nil
}

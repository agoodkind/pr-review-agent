package githubapp

import (
	"context"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type repositoryTreeUsage struct {
	entries int
	bytes   int64
	height  int
}

func boundRepositoryTrees(ctx context.Context, trees ...*object.Tree) error {
	known := make(map[plumbing.Hash]repositoryTreeUsage)

	for _, tree := range trees {
		_, err := countRepositoryTree(ctx, tree, 0, known)
		if err != nil {
			return err
		}
	}

	return nil
}

func countRepositoryTree(
	ctx context.Context,
	tree *object.Tree,
	depth int,
	known map[plumbing.Hash]repositoryTreeUsage,
) (repositoryTreeUsage, error) {
	var empty repositoryTreeUsage

	if depth > maximumRepositoryTreeDepth {
		return empty, errRepositoryTreeDepth
	}

	if cached, present := known[tree.Hash]; present {
		if cached.height > maximumRepositoryTreeDepth-depth {
			return empty, errRepositoryTreeDepth
		}

		return cached, nil
	}

	usage := repositoryTreeUsage{entries: len(tree.Entries), bytes: 0, height: 0}
	if usage.entries > maximumSnapshotEntries {
		return empty, errRepositoryTreeEntries
	}

	for _, entry := range tree.Entries {
		contextError := ctx.Err()
		if contextError != nil {
			return empty, repositoryAccessError("count repository tree paths", contextError)
		}

		switch entry.Mode {
		case filemode.Dir:
			child, err := tree.Tree(entry.Name)
			if err != nil {
				return empty, repositoryAccessError("read bounded child tree", err)
			}

			childUsage, err := countRepositoryTree(ctx, child, depth+1, known)
			if err != nil {
				return empty, err
			}

			if childUsage.entries > maximumSnapshotEntries-usage.entries {
				return empty, errRepositoryTreeEntries
			}

			if childUsage.bytes > maximumSnapshotExtractedBytes-usage.bytes {
				return empty, errRepositoryTreeBytes
			}

			usage.entries += childUsage.entries
			usage.bytes += childUsage.bytes
			usage.height = max(usage.height, childUsage.height+1)
		case filemode.Regular, filemode.Deprecated, filemode.Executable, filemode.Symlink:
			size, err := tree.Size(entry.Name)
			if err != nil {
				return empty, repositoryAccessError("count repository blob bytes", err)
			}

			if size < 0 || size > maximumSnapshotExtractedBytes-usage.bytes {
				return empty, errRepositoryTreeBytes
			}

			usage.bytes += size
		case filemode.Submodule:
		case filemode.Empty:
			return empty, errRepositoryFileMode
		default:
			return empty, errRepositoryFileMode
		}
	}

	known[tree.Hash] = usage

	return usage, nil
}

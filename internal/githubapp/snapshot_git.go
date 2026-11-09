package githubapp

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"goodkind.io/pr-review-agent/internal/domain"
)

const maximumRepositoryTreeDepth = 1024

func (client *Client) materializeRepositorySnapshot(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	ref domain.HeadSHA,
	parent string,
) (*repositorySnapshot, error) {
	var err error

	hash, err := immutableGitHash(ref)
	if err != nil {
		return nil, repositoryAccessError("materializeRepositorySnapshot", err)
	}

	cache := client.repositories

	err = cache.lock(ctx)
	if err != nil {
		return nil, repositoryAccessError("materializeRepositorySnapshot", err)
	}
	defer cache.unlock()

	key := repositoryCacheKey{installationID: installationID, repository: repo}

	entry, err := cache.open(key)
	if err != nil {
		return nil, repositoryAccessError("materializeRepositorySnapshot", err)
	}

	commit, err := client.fetchRepositoryCommit(ctx, key, entry, hash)
	if err != nil {
		return nil, repositoryAccessError("materializeRepositorySnapshot", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, repositoryAccessError("materializeRepositorySnapshot", err)
	}

	err = boundRepositoryTrees(ctx, tree)
	if err != nil {
		return nil, err
	}

	directory := filepath.Join(parent, string(ref))

	err = os.Mkdir(directory, 0o700)
	if err != nil {
		return nil, repositoryAccessError("materializeRepositorySnapshot", err)
	}

	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, repositoryAccessError("materializeRepositorySnapshot", err)
	}

	snapshot := &repositorySnapshot{
		root: root, files: nil, metadata: make(map[string]string), gitlinks: make(map[string]FileContent),
	}

	var (
		entries    int
		totalBytes int64
	)

	err = snapshot.writeGitTree(ctx, tree, "", 0, &entries, &totalBytes)
	if err == nil {
		err = loadSnapshotGitlinks(snapshot)
	}

	if err != nil {
		_ = root.Close()

		return nil, repositoryAccessError("materialize immutable Git snapshot", err)
	}

	slices.Sort(snapshot.files)

	return snapshot, nil
}

func (snapshot *repositorySnapshot) writeGitTree(
	ctx context.Context,
	tree *object.Tree,
	prefix string,
	depth int,
	entries *int,
	totalBytes *int64,
) error {
	if depth > maximumRepositoryTreeDepth {
		return errRepositoryTreeDepth
	}

	for _, entry := range tree.Entries {
		name, err := validateGitTreeEntry(ctx, entry, prefix, entries)
		if err != nil {
			return err
		}

		switch entry.Mode {
		case filemode.Dir:
			child, err := tree.Tree(entry.Name)
			if err != nil {
				return repositoryAccessError("writeGitTree", err)
			}

			err = snapshot.writeGitTree(ctx, child, name+"/", depth+1, entries, totalBytes)
			if err != nil {
				return repositoryAccessError("writeGitTree", err)
			}
		case filemode.Submodule:
			snapshot.gitlinks[name] = FileContent{Data: []byte("Subproject commit " + entry.Hash.String() + "\n"), Metadata: ""}
		case filemode.Regular, filemode.Deprecated, filemode.Executable, filemode.Symlink:
			file, err := tree.TreeEntryFile(&entry)
			if err != nil {
				return repositoryAccessError("writeGitTree", err)
			}

			if file.Size < 0 || file.Size > maximumSnapshotExtractedBytes-*totalBytes {
				return errRepositoryTreeBytes
			}

			*totalBytes += file.Size

			err = snapshot.writeGitBlob(file, name)
			if err != nil {
				return repositoryAccessError("writeGitTree", err)
			}

			if entry.Mode == filemode.Symlink {
				snapshot.metadata[name] = "Symbolic link target text; the snapshot does not follow this link."
			}
		case filemode.Empty:
			return errRepositoryFileMode
		default:
			return errRepositoryFileMode
		}
	}

	return nil
}

func (snapshot *repositorySnapshot) writeGitBlob(blob *object.File, name string) error {
	var err error

	err = snapshot.root.MkdirAll(path.Dir(name), 0o700)
	if err != nil {
		return repositoryAccessError("writeGitBlob", err)
	}

	source, err := blob.Reader()
	if err != nil {
		return repositoryAccessError("writeGitBlob", err)
	}

	defer func() { _ = source.Close() }()

	destination, err := snapshot.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return repositoryAccessError("writeGitBlob", err)
	}

	copied, copyErr := io.Copy(destination, source)

	closeErr := destination.Close()

	err = errors.Join(copyErr, closeErr)
	if err != nil {
		return repositoryAccessError("writeGitBlob", err)
	}

	if copied != blob.Size {
		return errRepositoryBlobSize
	}

	snapshot.files = append(snapshot.files, name)

	return nil
}

func validateGitTreeEntry(ctx context.Context, entry object.TreeEntry, prefix string, entries *int) (string, error) {
	err := ctx.Err()
	if err != nil {
		return "", repositoryAccessError("writeGitTree", err)
	}

	*entries++
	if *entries > maximumSnapshotEntries {
		return "", errRepositoryTreeEntries
	}

	if strings.Contains(entry.Name, "/") {
		return "", errRepositoryTreePath
	}

	name := prefix + entry.Name

	err = validateSnapshotPath(name)
	if err != nil {
		return "", repositoryAccessError("writeGitTree", err)
	}

	return name, nil
}

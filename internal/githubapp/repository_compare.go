package githubapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	udiff "github.com/aymanbagabas/go-udiff"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"goodkind.io/pr-review-agent/internal/domain"
)

func (client *Client) compareRepositoryCommits(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	base domain.HeadSHA,
	head domain.HeadSHA,
) (Comparison, error) {
	var err error

	baseHash, err := immutableGitHash(base)
	if err != nil {
		return Comparison{}, repositoryAccessError("compareRepositoryCommits", err)
	}

	headHash, err := immutableGitHash(head)
	if err != nil {
		return Comparison{}, repositoryAccessError("compareRepositoryCommits", err)
	}

	key := repositoryCacheKey{installationID: installationID, repository: repo}

	cache := &client.repositories

	err = cache.lock(ctx)
	if err != nil {
		return Comparison{}, repositoryAccessError("compareRepositoryCommits", err)
	}
	defer cache.unlock()

	entry, err := cache.open(key)
	if err != nil {
		return Comparison{}, repositoryAccessError("compareRepositoryCommits", err)
	}

	mergeBase, err := client.repositoryMergeBase(ctx, key, entry, baseHash, headHash)
	if err != nil {
		return Comparison{}, repositoryAccessError("compareRepositoryCommits", err)
	}

	baseTree, err := mergeBase.Tree()
	if err != nil {
		return Comparison{}, repositoryAccessError("compareRepositoryCommits", err)
	}

	headCommit, err := client.fetchRepositoryCommit(ctx, key, entry, headHash)
	if err != nil {
		return Comparison{}, repositoryAccessError("compareRepositoryCommits", err)
	}

	headTree, err := headCommit.Tree()
	if err != nil {
		return Comparison{}, repositoryAccessError("compareRepositoryCommits", err)
	}

	changes, err := object.DiffTreeWithOptions(ctx, baseTree, headTree, object.DefaultDiffTreeOptions)
	if err != nil {
		return Comparison{}, repositoryAccessError("compareRepositoryCommits", err)
	}

	comparison := Comparison{
		MergeBase: domain.HeadSHA(mergeBase.Hash.String()),
		Files:     make([]ChangedFile, 0, len(changes)),
	}
	for _, change := range changes {
		err = ctx.Err()
		if err != nil {
			return Comparison{}, repositoryAccessError("compareRepositoryCommits", err)
		}

		file, err := localChangedFile(change)
		if err != nil {
			return Comparison{}, repositoryAccessError("compareRepositoryCommits", err)
		}

		comparison.Files = append(comparison.Files, file)
	}

	return comparison, nil
}

const (
	initialAncestryDepth    = 16
	ancestryDepthMultiplier = 4
)

type ancestryState struct {
	colors uint8
	stale  bool
}

func (client *Client) repositoryMergeBase(
	ctx context.Context,
	key repositoryCacheKey,
	entry *cachedRepository,
	base plumbing.Hash,
	head plumbing.Hash,
) (*object.Commit, error) {
	var err error

	const (
		baseColor  uint8 = 1
		headColor  uint8 = 2
		bothColors       = baseColor | headColor
	)

	states := map[plumbing.Hash]ancestryState{base: {colors: baseColor, stale: false}}
	headState := states[head]
	headState.colors |= headColor
	states[head] = headState
	queue := []plumbing.Hash{base, head}
	depth := 1

	for len(queue) > 0 {
		err = ctx.Err()
		if err != nil {
			return nil, repositoryAccessError("repositoryMergeBase", err)
		}

		hash := queue[0]
		queue = queue[1:]
		state := states[hash]

		commit, nextDepth, loadErr := client.loadAncestryCommit(ctx, key, entry, hash, queue, states, depth)
		if loadErr != nil {
			return nil, loadErr
		}

		depth = nextDepth

		if commit == nil {
			continue
		}

		if len(states) > maximumSnapshotEntries {
			return nil, errRepositoryAncestryLimit
		}

		for _, parent := range commit.ParentHashes {
			before := states[parent]

			after := before
			if state.stale || state.colors == bothColors {
				after.stale = true
			} else {
				after.colors |= state.colors
			}

			if before != after {
				states[parent] = after
				queue = append(queue, parent)
			}
		}
	}

	var selected plumbing.Hash

	for hash, state := range states {
		if state.colors != bothColors || state.stale {
			continue
		}

		if selected != plumbing.ZeroHash {
			return nil, errRepositoryMultipleMergeBases
		}

		selected = hash
	}

	if selected == plumbing.ZeroHash {
		return nil, newAPIError(http.StatusUnprocessableEntity, "Git revisions do not share an ancestor")
	}

	return client.fetchRepositoryCommit(ctx, key, entry, selected)
}

func localChangedFile(change *object.Change) (ChangedFile, error) {
	var err error

	file := ChangedFile{
		Path: change.To.Name, Status: "modified", PatchPresent: true, ChangeCountsKnown: true,
		PreviousPath: "", Patch: "", Additions: 0, Deletions: 0,
	}
	switch {
	case change.From.Name == "":
		file.Status = "added"
	case change.To.Name == "":
		file.Status, file.Path = "removed", change.From.Name
	case change.From.Name != change.To.Name:
		file.Status, file.PreviousPath = "renamed", change.From.Name
	}

	err = validateSnapshotPath(file.Path)
	if err != nil {
		return ChangedFile{}, repositoryAccessError("localChangedFile", err)
	}

	previous, err := localChangeContent(change.From)
	if err != nil {
		return ChangedFile{}, repositoryAccessError("localChangedFile", err)
	}

	current, err := localChangeContent(change.To)
	if err != nil {
		return ChangedFile{}, repositoryAccessError("localChangedFile", err)
	}

	previousIsText := utf8.ValidString(previous) && !strings.ContainsRune(previous, '\x00')

	currentIsText := utf8.ValidString(current) && !strings.ContainsRune(current, '\x00')
	if !previousIsText || !currentIsText {
		file.Status, file.Patch = "binary", "Binary files base and head differ\n"

		return file, nil
	}

	patch, err := udiff.ToUnified("base", "head", previous, udiff.Lines(previous, current), udiff.DefaultContextLines)
	if err != nil {
		return ChangedFile{}, repositoryAccessError("localChangedFile", err)
	}

	file.Patch = patch
	inHunk := false

	for line := range strings.SplitSeq(patch, "\n") {
		if strings.HasPrefix(line, "@@ ") {
			inHunk = true

			continue
		}

		if !inHunk {
			continue
		}

		if strings.HasPrefix(line, "+") {
			file.Additions++
		}

		if strings.HasPrefix(line, "-") {
			file.Deletions++
		}
	}

	return file, nil
}

func localChangeContent(entry object.ChangeEntry) (string, error) {
	if entry.Name == "" {
		return "", nil
	}

	if entry.TreeEntry.Mode == filemode.Submodule {
		return "Subproject commit " + entry.TreeEntry.Hash.String() + "\n", nil
	}

	file, err := entry.Tree.TreeEntryFile(&entry.TreeEntry)
	if err != nil {
		return "", repositoryAccessError("localChangeContent", err)
	}

	if file.Size > maxResponseBytes {
		return "", errRepositoryContentLimit
	}

	reader, err := file.Reader()
	if err != nil {
		return "", repositoryAccessError("localChangeContent", err)
	}

	defer func() { _ = reader.Close() }()

	data, err := io.ReadAll(io.LimitReader(reader, maxResponseBytes+1))
	if err != nil {
		return "", repositoryAccessError("localChangeContent", err)
	}

	if len(data) > maxResponseBytes {
		return "", errRepositoryContentLimit
	}

	return string(data), nil
}

func missingRepositoryAncestors(
	entry *cachedRepository,
	states map[plumbing.Hash]ancestryState,
	hash plumbing.Hash,
	queue []plumbing.Hash,
) ([]plumbing.Hash, error) {
	var err error

	missing := []plumbing.Hash{hash}

	seen := map[plumbing.Hash]bool{hash: true}
	for _, pending := range queue {
		if seen[pending] || states[pending].stale {
			continue
		}

		_, err = object.GetCommit(entry.repository, pending)
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			missing = append(missing, pending)
			seen[pending] = true
		} else if err != nil {
			return nil, repositoryAccessError("repositoryMergeBase", err)
		}
	}

	return missing, nil
}

func (client *Client) loadAncestryCommit(
	ctx context.Context,
	key repositoryCacheKey,
	entry *cachedRepository,
	hash plumbing.Hash,
	queue []plumbing.Hash,
	states map[plumbing.Hash]ancestryState,
	depth int,
) (*object.Commit, int, error) {
	commit, err := object.GetCommit(entry.repository, hash)
	if states[hash].stale && errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, depth, nil
	}

	if err == nil {
		return commit, depth, nil
	}

	if !errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, depth, repositoryAccessError("load Git ancestry", err)
	}

	missing, err := missingRepositoryAncestors(entry, states, hash, queue)
	if err != nil {
		return nil, depth, err
	}

	err = client.fetchRepositoryAncestry(ctx, key, entry, missing, depth)
	if err != nil {
		return nil, depth, err
	}

	if depth == 1 {
		depth = initialAncestryDepth
	} else {
		depth = min(depth*ancestryDepthMultiplier, maximumSnapshotEntries)
	}

	commit, err = object.GetCommit(entry.repository, hash)
	if err != nil {
		return nil, depth, repositoryAccessError("load fetched Git ancestry", err)
	}

	return commit, depth, nil
}

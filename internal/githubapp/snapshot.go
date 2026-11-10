package githubapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"

	"goodkind.io/gklog"

	"goodkind.io/pr-review-agent/internal/domain"
)

type repositorySnapshotKey struct{}

type repositorySnapshotScope struct {
	client         *Client
	installationID int64
	repository     domain.Repository
	head           domain.HeadSHA
	gate           chan struct{}
	directory      string
	revisions      map[domain.HeadSHA]repositorySnapshotResult
	closed         bool
	pullRequest    *PullRequest
}

type repositorySnapshotResult struct {
	snapshot *repositorySnapshot
	err      error
}

type repositorySnapshot struct {
	root     *os.Root
	files    []string
	metadata map[string]string
	gitlinks map[string]FileContent
}

// OpenRepositorySnapshot registers private, lazy snapshots for one repository review.
func (client *Client) OpenRepositorySnapshot(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	head domain.HeadSHA,
) (context.Context, func() error, error) {
	if _, err := domain.ParseHeadSHA(string(head)); err != nil {
		client.logger.WarnContext(ctx, "Validate repository snapshot head", "err", err)
		return nil, nil, fmt.Errorf("repository snapshot requires an immutable head: %w", err)
	}

	if installationID <= 0 || repo.Owner == "" || repo.Name == "" {
		return nil, nil, errors.New("repository snapshot requires an installation and repository")
	}

	if ctx.Value(repositorySnapshotKey{}) != nil {
		return nil, nil, errors.New("repository snapshot scope already exists")
	}

	scope := &repositorySnapshotScope{
		client:         client,
		installationID: installationID,
		repository:     repo,
		head:           head,
		gate: make(
			chan struct{},
			1,
		),
		directory:   "",
		revisions:   make(map[domain.HeadSHA]repositorySnapshotResult),
		closed:      false,
		pullRequest: nil,
	}
	scope.gate <- struct{}{}

	return context.WithValue(ctx, repositorySnapshotKey{}, scope), scope.close, nil
}

func (client *Client) repositorySnapshotScope(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
) (*repositorySnapshotScope, error) {
	scope, ok := ctx.Value(repositorySnapshotKey{}).(*repositorySnapshotScope)
	if !ok {
		return nil, nil
	}

	if scope.client != client || scope.installationID != installationID ||
		scope.repository != repo {
		return nil, errors.New(
			"repository snapshot does not match the client, installation, and repository",
		)
	}

	return scope, nil
}

func (scope *repositorySnapshotScope) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		scope.client.logger.WarnContext(ctx, "Cancel repository snapshot access", "err", err)
		return fmt.Errorf("cancel repository snapshot access: %w", err)
	}

	select {
	case <-ctx.Done():
		err := ctx.Err()
		scope.client.logger.WarnContext(ctx, "Cancel repository snapshot wait", "err", err)

		return fmt.Errorf("cancel repository snapshot wait: %w", err)
	case <-scope.gate:
		if scope.closed {
			scope.gate <- struct{}{}
			return errors.New("repository snapshot scope is closed")
		}

		return nil
	}
}

func (scope *repositorySnapshotScope) unlock() { scope.gate <- struct{}{} }

func (scope *repositorySnapshotScope) close() error {
	<-scope.gate
	defer scope.unlock()

	if scope.closed {
		return nil
	}

	scope.closed = true

	var failures []error

	for _, revision := range scope.revisions {
		if revision.snapshot != nil {
			failures = append(failures, revision.snapshot.root.Close())
		}
	}

	if scope.directory != "" {
		failures = append(failures, os.RemoveAll(scope.directory))
	}

	if err := errors.Join(failures...); err != nil {
		slog.Warn("Close private repository snapshots", "err", err)
		return fmt.Errorf("close private repository snapshots: %w", err)
	}

	return nil
}

func (scope *repositorySnapshotScope) load(
	ctx context.Context,
	ref domain.HeadSHA,
) (*repositorySnapshot, error) {
	if _, err := domain.ParseHeadSHA(string(ref)); err != nil {
		scope.client.logger.WarnContext(ctx, "Validate repository snapshot revision", "err", err)
		return nil, fmt.Errorf("repository snapshot requires an immutable revision: %w", err)
	}

	if cached, ok := scope.revisions[ref]; ok {
		return cached.snapshot, cached.err
	}

	if scope.directory == "" {
		directory, err := os.MkdirTemp("", "pragent-repository-")
		if err != nil {
			scope.client.logger.WarnContext(
				ctx,
				"Create private repository snapshot directory",
				"err",
				err,
			)

			return nil, fmt.Errorf("create private repository snapshot directory: %w", err)
		}

		scope.directory = directory
	}

	snapshot,
		err := scope.client.materializeRepositorySnapshot(
		ctx,
		scope.installationID,
		scope.repository,
		ref,
		scope.directory,
	)
	scope.revisions[ref] = repositorySnapshotResult{snapshot: snapshot, err: err}

	return snapshot, err
}

func validateSnapshotPath(filePath string) error {
	if filePath == "" || filePath == "." || path.IsAbs(filePath) ||
		path.Clean(filePath) != filePath ||
		strings.HasPrefix(filePath, "../") ||
		strings.ContainsAny(filePath, "\\\x00") {
		return errors.New("repository snapshot requires an exact relative path")
	}

	return nil
}

func (scope *repositorySnapshotScope) read(
	ctx context.Context,
	filePath string,
	ref domain.HeadSHA,
) (FileContent, error) {
	if err := validateSnapshotPath(filePath); err != nil {
		return FileContent{}, err
	}

	if err := scope.lock(ctx); err != nil {
		return FileContent{}, err
	}

	defer scope.unlock()

	snapshot, err := scope.load(ctx, ref)
	if err != nil {
		return FileContent{}, err
	}

	if content, ok := snapshot.gitlinks[filePath]; ok {
		return content, nil
	}

	if _, exists := slices.BinarySearch(snapshot.files, filePath); !exists {
		return FileContent{}, newAPIError(
			http.StatusNotFound,
			"file does not exist in the pinned repository snapshot",
		)
	}

	info, err := snapshot.root.Stat(filePath)
	if err != nil {
		scope.client.logger.WarnContext(ctx, "Inspect private repository snapshot file", "err", err)
		return FileContent{}, fmt.Errorf("inspect private repository snapshot file: %w", err)
	}

	if info.Size() > maxResponseBytes {
		return FileContent{}, errors.New(
			"repository file exceeds the existing content response byte limit",
		)
	}

	content, err := snapshot.root.ReadFile(filePath)
	if err != nil {
		scope.client.logger.WarnContext(ctx, "Read private repository snapshot file", "err", err)
		return FileContent{}, fmt.Errorf("read private repository snapshot file: %w", err)
	}

	return FileContent{Data: content, Metadata: snapshot.metadata[filePath]}, nil
}

func (scope *repositorySnapshotScope) search(ctx context.Context, query string) ([]string, error) {
	if err := scope.lock(ctx); err != nil {
		return nil, err
	}

	defer scope.unlock()

	snapshot, err := scope.load(ctx, scope.head)
	if err != nil {
		return nil, err
	}

	var matches []string

	needle := []byte(query)
	buffer := make([]byte, max(32*1024, len(needle)*2))

	for _, filePath := range snapshot.files {
		if err := ctx.Err(); err != nil {
			scope.client.logger.WarnContext(ctx, "Cancel private repository search", "err", err)
			return nil, fmt.Errorf("cancel private repository search: %w", err)
		}

		matched, err := snapshot.contains(ctx, filePath, needle, buffer)
		if err != nil {
			scope.client.logger.WarnContext(
				ctx,
				"Search private repository snapshot file",
				"err",
				err,
			)

			return nil, fmt.Errorf("search private repository snapshot file: %w", err)
		}

		if matched {
			matches = append(matches, filePath)
		}
	}

	return matches, nil
}

func (snapshot *repositorySnapshot) contains(
	ctx context.Context,
	filePath string,
	query []byte,
	buffer []byte,
) (bool, error) {
	logger := gklog.L(ctx)

	file, err := snapshot.root.Open(filePath)
	if err != nil {
		logger.WarnContext(ctx, "Open repository snapshot search source", "err", err)
		return false, fmt.Errorf("open repository snapshot search source: %w", err)
	}

	defer func() { _ = file.Close() }()

	retained := 0

	for {
		if err := ctx.Err(); err != nil {
			logger.WarnContext(ctx, "Cancel repository snapshot content search", "err", err)
			return false, fmt.Errorf("cancel repository snapshot content search: %w", err)
		}

		count, readErr := file.Read(buffer[retained:])

		available := retained + count
		if bytes.Contains(buffer[:available], query) {
			return true, nil
		}

		if errors.Is(readErr, io.EOF) {
			return false, nil
		}

		if readErr != nil {
			logger.WarnContext(ctx, "Read repository snapshot search source", "err", readErr)
			return false, fmt.Errorf("read repository snapshot search source: %w", readErr)
		}

		retained = min(available, len(query)-1)
		copy(buffer, buffer[available-retained:available])
	}
}

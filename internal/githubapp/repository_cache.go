package githubapp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-git/go-billy/v5/osfs"
	gitcache "github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"

	"goodkind.io/pr-review-agent/internal/domain"
)

var (
	errRepositoryCacheClosed        = errors.New("repository cache is closed")
	errRepositoryCacheFull          = errors.New("repository Git object cache exceeds its disk byte limit")
	errRepositoryEndpoint           = errors.New("invalid repository Git HTTPS endpoint")
	errRepositoryShallowUnsupported = errors.New("repository Git server must support bounded shallow acquisition")
	errRepositoryTransferLimit      = errors.New("repository Git transfer exceeds its compressed byte limit")
	errRepositoryPackEntries        = errors.New("repository Git pack exceeds its object entry limit")
	errRepositoryPackBytes          = errors.New("repository Git pack exceeds its extracted byte limit")
	errRepositoryAncestryLimit      = errors.New("repository Git comparison exceeds its ancestry entry limit")
	errRepositoryContentLimit       = errors.New("repository file exceeds the existing content response byte limit")
	errRepositoryTreeDepth          = errors.New("repository tree exceeds its depth limit")
	errRepositoryTreeEntries        = errors.New("repository tree exceeds its entry limit")
	errRepositoryTreePath           = errors.New("repository tree entry contains a path separator")
	errRepositoryTreeBytes          = errors.New("repository tree exceeds its extracted byte limit")
	errRepositoryFileMode           = errors.New("repository tree contains an unsupported file mode")
	errRepositoryBlobSize           = errors.New("repository Git blob does not match its declared size")
)

const (
	maximumCachedRepositories  = 32
	repositoryMemoryCacheBytes = 1024 * 1024
)

type repositoryCacheKey struct {
	installationID int64
	repository     domain.Repository
}

type cachedRepository struct {
	repository *filesystem.Storage
	directory  string
	size       int64
	used       uint64
}

type repositoryCache struct {
	once      sync.Once
	gate      chan struct{}
	directory string
	entries   map[repositoryCacheKey]*cachedRepository
	sequence  uint64
	closed    bool
}

func (cache *repositoryCache) initialize() {
	cache.once.Do(func() {
		cache.gate = make(chan struct{}, 1)
		cache.gate <- struct{}{}

		cache.entries = make(map[repositoryCacheKey]*cachedRepository)
	})
}

func (cache *repositoryCache) lock(ctx context.Context) error {
	cache.initialize()

	err := ctx.Err()
	if err != nil {
		return repositoryAccessError("lock", err)
	}

	select {
	case <-ctx.Done():
		return repositoryAccessError("wait for Git cache", ctx.Err())
	case <-cache.gate:
		if cache.closed {
			cache.unlock()

			return errRepositoryCacheClosed
		}

		return nil
	}
}

func (cache *repositoryCache) unlock() { cache.gate <- struct{}{} }

func (cache *repositoryCache) open(key repositoryCacheKey) (*cachedRepository, error) {
	var err error

	cache.sequence++
	if entry := cache.entries[key]; entry != nil {
		entry.used = cache.sequence

		return entry, nil
	}

	if cache.directory == "" {
		directory, err := os.MkdirTemp("", "pragent-git-cache-")
		if err != nil {
			return nil, repositoryAccessError("create private Git cache", err)
		}

		cache.directory = directory
	}

	if len(cache.entries) >= maximumCachedRepositories {
		err = cache.evict(nil)
		if err != nil {
			return nil, repositoryAccessError("open", err)
		}
	}

	directory, err := os.MkdirTemp(cache.directory, "repository-")
	if err != nil {
		return nil, repositoryAccessError("open", err)
	}

	repository := filesystem.NewStorageWithOptions(
		osfs.New(directory), gitcache.NewObjectLRU(repositoryMemoryCacheBytes),
		filesystem.Options{LargeObjectThreshold: maxResponseBytes},
	)

	err = repository.Init()
	if err != nil {
		return nil, repositoryAccessError("open", err)
	}

	entry := &cachedRepository{repository: repository, directory: directory, used: cache.sequence, size: 0}
	cache.entries[key] = entry

	return entry, nil
}

func (cache *repositoryCache) evict(active *cachedRepository) error {
	slog.Debug("Evict inactive repository Git objects")

	var (
		oldest   *cachedRepository
		selected repositoryCacheKey
	)

	for key, entry := range cache.entries {
		if entry != active && (oldest == nil || entry.used < oldest.used) {
			oldest, selected = entry, key
		}
	}

	if oldest == nil {
		return errRepositoryCacheFull
	}

	err := os.RemoveAll(oldest.directory)
	if err != nil {
		return repositoryAccessError("evict inactive Git cache", err)
	}

	delete(cache.entries, selected)

	return nil
}

func (cache *repositoryCache) bound(active *cachedRepository) error {
	size, err := repositoryDiskBytes(active.directory)
	if err != nil {
		return repositoryAccessError("bound", err)
	}

	active.size = size

	for {
		var total int64
		for _, entry := range cache.entries {
			total += entry.size
		}

		if total <= maximumSnapshotCompressedBytes {
			return nil
		}

		err := cache.evict(active)
		if err != nil {
			return repositoryAccessError("bound", err)
		}
	}
}

func repositoryDiskBytes(directory string) (int64, error) {
	var size int64

	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() {
			return nil
		}

		info, err := os.Stat(path)
		if err != nil {
			return repositoryAccessError("repositoryDiskBytes", err)
		}

		size += info.Size()

		return nil
	})
	if err != nil {
		return 0, repositoryAccessError("measure Git cache", err)
	}

	return size, nil
}

// Close deletes the client's private Git object cache after its readers finish.
//
// Close waits for the current Git operation and accepts repeated calls.
//
// Example:
//
//	defer client.Close()
func (client *Client) Close() error {
	cache := client.repositories
	cache.initialize()

	<-cache.gate
	defer cache.unlock()

	if cache.closed {
		return nil
	}

	client.logger.Debug("Close private Git object cache")

	cache.closed = true
	if cache.directory == "" {
		return nil
	}

	err := os.RemoveAll(cache.directory)
	if err != nil {
		return repositoryAccessError("close private Git object cache", err)
	}

	return nil
}

func repositoryAccessError(operation string, cause error) error {
	slog.Warn(operation, "err", cause)

	return fmt.Errorf("%s: %w", operation, cause)
}

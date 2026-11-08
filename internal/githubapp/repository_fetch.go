package githubapp

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"goodkind.io/pr-review-agent/internal/domain"
)

const (
	maximumSnapshotCompressedBytes = 512 * 1024 * 1024
	maximumSnapshotExtractedBytes  = 2 * 1024 * 1024 * 1024
	maximumSnapshotEntries         = 200000
	repositoryWarmCacheDepth       = 2
)

func (client *Client) fetchRepositoryCommit(
	ctx context.Context,
	key repositoryCacheKey,
	entry *cachedRepository,
	hash plumbing.Hash,
) (*object.Commit, error) {
	var err error

	commit, err := object.GetCommit(entry.repository, hash)
	if err == nil {
		return commit, nil
	}

	if !errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, repositoryAccessError("fetchRepositoryCommit", err)
	}

	err = client.fetchRepositoryAncestry(ctx, key, entry, []plumbing.Hash{hash}, 1)
	if err != nil {
		return nil, repositoryAccessError("fetchRepositoryCommit", err)
	}

	commit, err = object.GetCommit(entry.repository, hash)
	if err != nil {
		return nil, repositoryAccessError("load exact Git commit", err)
	}

	return commit, nil
}

func (client *Client) fetchRepositoryAncestry(
	ctx context.Context,
	key repositoryCacheKey,
	entry *cachedRepository,
	hashes []plumbing.Hash,
	depth int,
) error {
	client.logger.DebugContext(ctx, "Fetch immutable repository Git objects",
		"revision_count", len(hashes), "depth", depth,
	)

	token, err := client.installationToken(ctx, key.installationID)
	if err != nil {
		return repositoryAccessError("fetchRepositoryAncestry", err)
	}

	address := "https://github.com/" + key.repository.Owner + "/" + key.repository.Name + ".git"

	endpoint, err := transport.NewEndpoint(address)
	if err != nil {
		return errRepositoryEndpoint
	}

	authentication := &githttp.BasicAuth{Username: "x-access-token", Password: token}

	session, err := githttp.NewClient(client.httpClient).NewUploadPackSession(endpoint, authentication)
	if err != nil {
		return repositoryAccessError("open authenticated Git session", err)
	}

	defer func() { _ = session.Close() }()

	advertised, err := session.AdvertisedReferencesContext(ctx)
	if err != nil {
		return repositoryAccessError("read Git references", err)
	}

	if !advertised.Capabilities.Supports(capability.Shallow) {
		return errRepositoryShallowUnsupported
	}

	request, err := repositoryUploadRequest(entry, hashes, depth, advertised)
	if err != nil {
		return err
	}

	response, err := session.UploadPack(ctx, request)
	if err != nil {
		return repositoryAccessError("fetch immutable Git revision", err)
	}

	defer func() { _ = response.Close() }()

	return client.storeRepositoryPack(ctx, key, entry, request, response, hashes)
}

func repositoryUploadRequest(
	entry *cachedRepository,
	hashes []plumbing.Hash,
	depth int,
	advertised *packp.AdvRefs,
) (*packp.UploadPackRequest, error) {
	var err error

	request := packp.NewUploadPackRequest()
	request.Wants = hashes
	request.Depth = packp.DepthCommits(depth)

	request.Shallows, err = entry.repository.Shallow()
	if err != nil {
		return nil, repositoryAccessError("fetchRepositoryAncestry", err)
	}

	references, err := entry.repository.IterReferences()
	if err != nil {
		return nil, repositoryAccessError("fetchRepositoryAncestry", err)
	}

	err = references.ForEach(func(reference *plumbing.Reference) error {
		if reference.Type() == plumbing.HashReference {
			request.Haves = append(request.Haves, reference.Hash())
		}

		return nil
	})
	if err != nil {
		return nil, repositoryAccessError("fetchRepositoryAncestry", err)
	}
	// Include a cached parent in the shallow graph to negotiate its existing objects.
	if depth == 1 && len(request.Haves) > 0 {
		request.Depth = packp.DepthCommits(repositoryWarmCacheDepth)
	}

	if advertised.Capabilities.Supports(capability.NoProgress) {
		err = request.Capabilities.Set(capability.NoProgress)
		if err != nil {
			return nil, repositoryAccessError("fetchRepositoryAncestry", err)
		}
	}

	err = request.Capabilities.Set(capability.Shallow)
	if err != nil {
		return nil, repositoryAccessError("fetchRepositoryAncestry", err)
	}

	if advertised.Capabilities.Supports(capability.OFSDelta) {
		err = request.Capabilities.Set(capability.OFSDelta)
		if err != nil {
			return nil, repositoryAccessError("fetchRepositoryAncestry", err)
		}
	}

	return request, nil
}

func (client *Client) storeRepositoryPack(
	ctx context.Context,
	key repositoryCacheKey,
	entry *cachedRepository,
	request *packp.UploadPackRequest,
	response *packp.UploadPackResponse,
	hashes []plumbing.Hash,
) error {
	client.logger.DebugContext(ctx, "Store fetched repository Git objects", "revision_count", len(hashes))

	var err error

	staging, err := os.CreateTemp(client.repositories.directory, "incoming-*.pack")
	if err != nil {
		return repositoryAccessError("fetchRepositoryAncestry", err)
	}

	defer func() { _ = staging.Close(); _ = os.Remove(staging.Name()) }()

	limited := &io.LimitedReader{R: response, N: maximumSnapshotCompressedBytes + 1}

	_, err = io.Copy(staging, limited)
	if err != nil {
		return repositoryAccessError("receive Git pack", err)
	}

	if limited.N <= 0 {
		return errRepositoryTransferLimit
	}

	_, err = staging.Seek(0, io.SeekStart)
	if err != nil {
		return repositoryAccessError("fetchRepositoryAncestry", err)
	}

	observer := &repositoryPackBounds{checkContext: ctx.Err, bytes: 0}

	parser, err := packfile.NewParser(packfile.NewScanner(staging), observer)
	if err != nil {
		return repositoryAccessError("fetchRepositoryAncestry", err)
	}

	_, err = parser.Parse()
	if err != nil {
		return repositoryAccessError("validate bounded Git pack", err)
	}

	_, err = staging.Seek(0, io.SeekStart)
	if err != nil {
		return repositoryAccessError("fetchRepositoryAncestry", err)
	}

	err = packfile.UpdateObjectStorage(entry.repository, staging)
	if err != nil {
		return repositoryAccessError("store Git pack", err)
	}

	for _, hash := range hashes {
		_, err = object.GetCommit(entry.repository, hash)
		if err != nil {
			return repositoryAccessError("Git server did not provide the exact requested commit", err)
		}

		reference := plumbing.NewHashReference(plumbing.ReferenceName("refs/pragent/"+hash.String()), hash)

		err = entry.repository.SetReference(reference)
		if err != nil {
			return repositoryAccessError("fetchRepositoryAncestry", err)
		}
	}

	shallows := slices.Clone(request.Shallows)
	for _, hash := range response.Shallows {
		if !slices.Contains(shallows, hash) {
			shallows = append(shallows, hash)
		}
	}

	shallows = slices.DeleteFunc(shallows, func(hash plumbing.Hash) bool {
		return slices.Contains(response.Unshallows, hash)
	})

	err = entry.repository.SetShallow(shallows)
	if err != nil {
		return repositoryAccessError("fetchRepositoryAncestry", err)
	}

	err = client.repositories.bound(entry)
	if err != nil {
		delete(client.repositories.entries, key)

		cleanupErr := os.RemoveAll(entry.directory)

		return repositoryAccessError("enforce Git cache size", errors.Join(err, cleanupErr))
	}

	return nil
}

type repositoryPackBounds struct {
	checkContext func() error
	bytes        int64
}

func (bounds *repositoryPackBounds) OnHeader(count uint32) error {
	if count > maximumSnapshotEntries {
		return errRepositoryPackEntries
	}

	return bounds.checkContext()
}

func (bounds *repositoryPackBounds) OnInflatedObjectHeader(_ plumbing.ObjectType, size int64, _ int64) error {
	if size < 0 || size > maximumSnapshotExtractedBytes-bounds.bytes {
		return errRepositoryPackBytes
	}

	bounds.bytes += size

	return bounds.checkContext()
}

func (bounds *repositoryPackBounds) OnInflatedObjectContent(_ plumbing.Hash, _ int64, _ uint32, _ []byte) error {
	return bounds.checkContext()
}
func (bounds *repositoryPackBounds) OnFooter(_ plumbing.Hash) error { return bounds.checkContext() }

func immutableGitHash(ref domain.HeadSHA) (plumbing.Hash, error) {
	var err error

	_, err = domain.ParseHeadSHA(string(ref))
	if err != nil {
		return plumbing.ZeroHash, repositoryAccessError("immutableGitHash", err)
	}

	return plumbing.NewHash(string(ref)), nil
}

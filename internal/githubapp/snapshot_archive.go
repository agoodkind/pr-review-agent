package githubapp

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"goodkind.io/gklog"

	"goodkind.io/pr-review-agent/internal/domain"
)

const (
	maximumSnapshotCompressedBytes = 512 * 1024 * 1024
	maximumSnapshotExtractedBytes  = 2 * 1024 * 1024 * 1024
	maximumSnapshotEntries         = 200000
	maximumSnapshotRedirects       = 3
)

func (client *Client) downloadRepositorySnapshot(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	ref domain.HeadSHA,
	parent string,
) (*repositorySnapshot, error) {
	response, err := client.openRepositoryArchive(ctx, installationID, repo, ref)
	if err != nil {
		return nil, err
	}

	defer func() { _ = response.Body.Close() }()
	directory := filepath.Join(parent, string(ref))
	if err := os.Mkdir(directory, 0o700); err != nil {
		client.logger.WarnContext(ctx, "Create repository revision directory", "err", err)
		return nil, fmt.Errorf("create repository revision directory: %w", err)
	}

	root, err := os.OpenRoot(directory)
	if err != nil {
		client.logger.WarnContext(ctx, "Open repository revision directory", "err", err)
		return nil, fmt.Errorf("open repository revision directory: %w", err)
	}

	snapshot, err := extractRepositoryArchive(ctx, response.Body, root)
	if err == nil {
		err = client.loadSnapshotGitlinks(ctx, installationID, repo, ref, snapshot)
	}

	if err != nil {
		client.logger.WarnContext(ctx, "Extract immutable repository snapshot", "err", err)
		_ = root.Close()

		return nil, fmt.Errorf("extract immutable repository snapshot: %w", err)
	}

	return snapshot, nil
}

func (client *Client) openRepositoryArchive(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	ref domain.HeadSHA,
) (*http.Response, error) {
	token, err := client.installationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}

	address, err := url.Parse(client.restURL(client.repoPath(repo, "/tarball/"+string(ref))))
	if err != nil {
		return nil, errors.New("repository archive API URL is invalid")
	}

	apiHost := address.Host
	transport := *client.httpClient
	transport.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

	for range maximumSnapshotRedirects {
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			address.String(),
			http.NoBody,
		)
		if err != nil {
			return nil, errors.New("create repository archive request")
		}

		request.Header.Set("Accept", githubAcceptHeader)
		request.Header.Set(githubAPIVersionHeader, githubAPIVersionValue)
		if address.Host == apiHost {
			request.Header.Set("Authorization", "Bearer "+token)
		}

		response, err := transport.Do(request)
		if err != nil {
			cause := err
			var transportError *url.Error
			if errors.As(err, &transportError) {
				cause = transportError.Err
			}

			client.logger.WarnContext(ctx, "Download repository archive", "err", cause)

			return nil, fmt.Errorf("download repository archive: %w", cause)
		}

		if response.StatusCode == http.StatusOK {
			return response, nil
		}

		if slices.Contains(
			[]int{
				http.StatusMovedPermanently,
				http.StatusFound,
				http.StatusSeeOther,
				http.StatusTemporaryRedirect,
				http.StatusPermanentRedirect,
			},
			response.StatusCode,
		) {
			address, err = response.Location()
			_ = response.Body.Close()
			if err != nil || address.Scheme != "https" || address.User != nil ||
				address.Host != "codeload.github.com" {
				return nil, errors.New(
					"repository archive redirect must use public GitHub codeload HTTPS",
				)
			}

			continue
		}

		body, readErr := readLimitedBody(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			client.logger.WarnContext(ctx, "Read repository archive refusal", "err", readErr)
			return nil, fmt.Errorf("read repository archive refusal: %w", readErr)
		}

		return nil, newAPIError(response.StatusCode, sanitizeErrorBody(body))
	}

	return nil, errors.New("repository archive exceeded its redirect limit")
}

func extractRepositoryArchive(
	ctx context.Context,
	body io.Reader,
	root *os.Root,
) (*repositorySnapshot, error) {
	logger := gklog.L(ctx)
	compressed := &io.LimitedReader{R: body, N: maximumSnapshotCompressedBytes + 1}
	decompressed, err := gzip.NewReader(compressed)
	if err != nil {
		logger.WarnContext(ctx, "Open repository archive gzip", "err", err)
		return nil, fmt.Errorf("open repository archive gzip: %w", err)
	}

	defer func() { _ = decompressed.Close() }()
	uncompressed := &io.LimitedReader{
		R: decompressed,
		N: maximumSnapshotExtractedBytes + maximumSnapshotEntries*1024 + 1,
	}
	archive := tar.NewReader(uncompressed)
	snapshot := &repositorySnapshot{
		root:     root,
		files:    nil,
		metadata: make(map[string]string),
		gitlinks: make(map[string]FileContent),
	}
	var archiveRoot string
	var totalBytes int64

	for entries := 0; ; entries++ {
		if err := ctx.Err(); err != nil {
			logger.WarnContext(ctx, "Cancel repository archive extraction", "err", err)
			return nil, fmt.Errorf("cancel repository archive extraction: %w", err)
		}

		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			logger.WarnContext(ctx, "Read repository archive entry", "err", err)
			return nil, fmt.Errorf("read repository archive entry: %w", err)
		}

		if entries >= maximumSnapshotEntries || compressed.N <= 0 {
			return nil, errors.New("repository archive exceeds its entry or compressed byte limit")
		}

		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		filePath, err := snapshotArchivePath(header.Name, &archiveRoot)
		if err != nil {
			return nil, err
		}

		if filePath == "" && header.Typeflag == tar.TypeDir {
			continue
		}

		if err := extractSnapshotEntry(
			ctx,
			archive,
			snapshot,
			header,
			filePath,
			&totalBytes,
		); err != nil {
			return nil, err
		}
	}

	if err := finishSnapshotArchive(ctx, archiveRoot, compressed, uncompressed); err != nil {
		return nil, err
	}

	slices.Sort(snapshot.files)

	return snapshot, nil
}

func finishSnapshotArchive(
	ctx context.Context,
	archiveRoot string,
	compressed *io.LimitedReader,
	uncompressed *io.LimitedReader,
) error {
	logger := gklog.L(ctx)
	if archiveRoot == "" {
		return errors.New("repository archive does not contain a repository directory")
	}

	if _, err := io.Copy(io.Discard, uncompressed); err != nil {
		logger.WarnContext(ctx, "Finish repository archive gzip", "err", err)
		return fmt.Errorf("finish repository archive gzip: %w", err)
	}

	if compressed.N <= 0 || uncompressed.N <= 0 {
		return errors.New("repository archive exceeds its compressed or extracted byte limit")
	}

	return nil
}

func snapshotArchivePath(name string, archiveRoot *string) (string, error) {
	clean := strings.TrimSuffix(name, "/")
	if err := validateSnapshotPath(clean); err != nil {
		return "", err
	}

	root, filePath, _ := strings.Cut(clean, "/")
	if *archiveRoot == "" {
		*archiveRoot = root
	}

	if root != *archiveRoot {
		return "", errors.New("repository archive contains multiple root directories")
	}

	return filePath, nil
}

func extractSnapshotEntry(
	ctx context.Context,
	archive *tar.Reader,
	snapshot *repositorySnapshot,
	header *tar.Header,
	filePath string,
	totalBytes *int64,
) error {
	logger := gklog.L(ctx)
	if header.Typeflag == tar.TypeDir {
		if err := snapshot.root.MkdirAll(filePath, 0o700); err != nil {
			logger.WarnContext(ctx, "Create repository archive directory", "err", err)
			return fmt.Errorf("create repository archive directory: %w", err)
		}

		return nil
	}

	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeSymlink {
		return errors.New("repository archive contains an unsupported link or special file")
	}

	if err := validateSnapshotPath(filePath); err != nil {
		return err
	}

	if _, duplicate := snapshot.metadata[filePath]; duplicate {
		return errors.New("repository archive contains a duplicate file")
	}

	size := header.Size
	if header.Typeflag == tar.TypeSymlink {
		size = int64(len(header.Linkname))
	}

	if size < 0 || size > maximumSnapshotExtractedBytes-*totalBytes {
		return errors.New("repository archive exceeds its extracted byte limit")
	}

	*totalBytes += size
	if err := snapshot.root.MkdirAll(path.Dir(filePath), 0o700); err != nil {
		logger.WarnContext(ctx, "Create repository archive file directory", "err", err)
		return fmt.Errorf("create repository archive file directory: %w", err)
	}

	return writeSnapshotEntry(ctx, archive, snapshot, header, filePath, size)
}

func writeSnapshotEntry(
	ctx context.Context,
	archive *tar.Reader,
	snapshot *repositorySnapshot,
	header *tar.Header,
	filePath string,
	size int64,
) error {
	logger := gklog.L(ctx)
	file, err := snapshot.root.OpenFile(filePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		logger.WarnContext(ctx, "Create repository archive file", "err", err)
		return fmt.Errorf("create repository archive file: %w", err)
	}

	var copied int64
	if header.Typeflag == tar.TypeSymlink {
		copied, err = io.Copy(file, strings.NewReader(header.Linkname))
		snapshot.metadata[filePath] = "Symbolic link target text; the snapshot does not follow this link."
	} else {
		copied, err = io.Copy(file, archive)
		snapshot.metadata[filePath] = ""
	}

	closeErr := file.Close()
	if err != nil {
		logger.WarnContext(ctx, "Write repository archive file", "err", err)
		return fmt.Errorf("write repository archive file: %w", err)
	}

	if closeErr != nil {
		logger.WarnContext(ctx, "Close repository archive file", "err", closeErr)
		return fmt.Errorf("close repository archive file: %w", closeErr)
	}

	if copied != size {
		return errors.New("repository archive file does not match its declared size")
	}

	snapshot.files = append(snapshot.files, filePath)

	return nil
}

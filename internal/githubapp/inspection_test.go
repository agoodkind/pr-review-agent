package githubapp_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

type snapshotArchiveEntry struct {
	header tar.Header
	data   string
}

func snapshotTestArchive(t *testing.T, entries []snapshotArchiveEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)

	for _, entry := range entries {
		if err := archive.WriteHeader(&entry.header); err != nil {
			t.Fatal(err)
		}

		if _, err := io.WriteString(archive, entry.data); err != nil {
			t.Fatal(err)
		}
	}

	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}

	return buffer.Bytes()
}

func snapshotTestClient(t *testing.T, handler http.HandlerFunc) *githubapp.Client {
	t.Helper()
	server := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/app/installations/77/access_tokens" {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(writer).Encode(map[string]string{
					"token":      "snapshot-test-token",
					"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
				})

				return
			}

			if strings.Contains(request.URL.Path, "/contents/") ||
				request.URL.Path == "/search/code" {
				t.Errorf(
					"Repository snapshot attempted a forbidden per-file or indexed request: %s",
					request.URL.Path,
				)
				http.Error(
					writer,
					"per-file and indexed requests are forbidden",
					http.StatusForbidden,
				)

				return
			}

			handler(writer, request)
		}),
	)
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	var cfg config.Config
	cfg.GitHubAPIBaseURL = address
	cfg.GitHubAppID = 12345
	examplePrivateKey := testPrivateKey(t)
	cfg.GitHubPrivateKey = examplePrivateKey

	return githubapp.NewClient(
		cfg,
		server.Client(),
		time.Now,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

func TestRepositorySnapshotUsesOnePrivateArchivePerPinnedRevision(t *testing.T) {
	temporary := t.TempDir()
	t.Setenv("TMPDIR", temporary)
	repo := domain.Repository{Owner: "owner", Name: "repository"}
	head := domain.HeadSHA(strings.Repeat("a", 40))
	previous := domain.HeadSHA(strings.Repeat("b", 40))
	query := `bridge " OR repo:foreign/repository`
	current := snapshotTestArchive(
		t,
		[]snapshotArchiveEntry{
			{
				header: tar.Header{
					Name:     "fixture/source.go",
					Typeflag: tar.TypeReg,
					Size:     int64(len(query)),
					Mode:     0o600,
				},
				data: query,
			},
			{
				header: tar.Header{
					Name:     "fixture/link.go",
					Typeflag: tar.TypeSymlink,
					Linkname: "source.go",
				},
				data: "",
			},
		},
	)
	old := snapshotTestArchive(
		t,
		[]snapshotArchiveEntry{
			{
				header: tar.Header{
					Name:     "fixture/source.go",
					Typeflag: tar.TypeReg,
					Size:     3,
					Mode:     0o600,
				},
				data: "old",
			},
		},
	)
	var mutex sync.Mutex
	counts := make(map[string]int)
	client := snapshotTestClient(
		t,
		func(writer http.ResponseWriter, request *http.Request) {
			if request.Header.Get("Authorization") != "Bearer snapshot-test-token" {
				t.Error("The archive request did not authenticate with the installation token.")
			}

			mutex.Lock()
			counts[request.URL.Path]++
			mutex.Unlock()
			switch request.URL.Path {
			case "/repos/owner/repository/tarball/" + string(head):
				_, _ = writer.Write(current)
			case "/repos/owner/repository/tarball/" + string(previous):
				_, _ = writer.Write(old)
			default:
				http.Error(writer, "unexpected repository request", http.StatusNotFound)
			}
		},
	)
	ctx, closeSnapshot, err := client.OpenRepositorySnapshot(t.Context(), 77, repo, head)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = closeSnapshot() })
	if len(counts) != 0 {
		t.Fatal("Registering an unused snapshot scope downloaded repository contents.")
	}

	for range 3 {
		content, err := client.GetFileContent(ctx, 77, repo, "source.go", head)
		if err != nil || string(content.Data) != query {
			t.Fatalf("The pinned snapshot did not provide current file contents: %v", err)
		}
	}

	paths, err := client.SearchRepository(ctx, 77, repo, query)
	if err != nil || len(paths) != 1 || paths[0] != "source.go" {
		t.Fatalf("Local repository search did not treat the query as literal: %v, %v", paths, err)
	}

	link, err := client.GetFileContent(ctx, 77, repo, "link.go", head)
	if err != nil || string(link.Data) != "source.go" ||
		!strings.Contains(link.Metadata, "Symbolic link target text") {
		t.Fatalf("The snapshot did not preserve the Git symlink blob: %v", err)
	}

	content, err := client.GetFileContent(ctx, 77, repo, "source.go", previous)
	if err != nil || string(content.Data) != "old" {
		t.Fatalf("The snapshot did not provide the pinned historical file: %v", err)
	}

	if _, err := client.GetFileContent(ctx, 78, repo, "source.go", head); err == nil {
		t.Fatal("The snapshot accepted another installation identity.")
	}

	if _, err := client.GetFileContent(ctx, 77, repo, "../source.go", head); err == nil {
		t.Fatal("The snapshot accepted a parent-directory read.")
	}

	if _, err := client.GetFileContent(ctx, 77, repo, "missing.go", head); err == nil {
		t.Fatal("The snapshot did not report a missing file.")
	}

	if _, err := client.SearchRepository(t.Context(), 77, repo, query); err == nil {
		t.Fatal("Repository search accepted a request without a snapshot scope.")
	}

	mutex.Lock()
	if len(counts) != 2 || counts["/repos/owner/repository/tarball/"+string(head)] != 1 ||
		counts["/repos/owner/repository/tarball/"+string(previous)] != 1 {
		t.Errorf("Snapshot reads did not reuse one download per immutable revision: %v", counts)
	}

	mutex.Unlock()
	directories, err := filepath.Glob(filepath.Join(temporary, "pragent-repository-*"))
	if err != nil || len(directories) != 1 {
		t.Fatalf("The snapshot did not create its private review directory: %v", err)
	}

	if err := filepath.WalkDir(
		directories[0],
		func(_ string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}

			if entry.Type()&os.ModeSymlink != 0 {
				t.Error("The private snapshot created an actual filesystem symlink.")
			}

			return nil
		},
	); err != nil {
		t.Fatal(err)
	}

	if err := closeSnapshot(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(directories[0]); !os.IsNotExist(err) {
		t.Fatal("Closing the review did not remove its private snapshot.")
	}

	if _, err := client.GetFileContent(ctx, 77, repo, "source.go", head); err == nil {
		t.Fatal("The closed scope resumed repository reads.")
	}
}

func TestRepositorySnapshotRejectsUnsafeArchiveEntries(t *testing.T) {
	for _, header := range []tar.Header{
		{Name: "fixture/../../escape", Typeflag: tar.TypeReg},
		{Name: "/absolute/source.go", Typeflag: tar.TypeReg},
		{Name: "fixture/link", Typeflag: tar.TypeLink, Linkname: "outside"},
		{Name: "fixture/device", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3},
	} {
		t.Run(
			header.Name,
			func(t *testing.T) {
				archive := snapshotTestArchive(
					t,
					[]snapshotArchiveEntry{
						{
							header: tar.Header{
								Name:     "fixture/source.go",
								Typeflag: tar.TypeReg,
								Size:     5,
								Mode:     0o600,
							},
							data: "valid",
						},
						{header: header, data: ""},
					},
				)
				client := snapshotTestClient(
					t,
					func(writer http.ResponseWriter, _ *http.Request) {
						_, _ = writer.Write(archive)
					},
				)
				repo := domain.Repository{Owner: "owner", Name: "repository"}
				head := domain.HeadSHA(strings.Repeat("a", 40))
				ctx, closeSnapshot, err := client.OpenRepositorySnapshot(
					t.Context(),
					77,
					repo,
					head,
				)
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(func() { _ = closeSnapshot() })
				content, err := client.GetFileContent(ctx, 77, repo, "source.go", head)
				var apiError githubapp.APIError
				if err == nil || len(content.Data) != 0 || errors.As(err, &apiError) {
					t.Fatalf(
						"The malformed archive did not prevent reading its existing source file: %v",
						err,
					)
				}
			},
		)
	}
}

func TestRepositorySnapshotPreservesCancellationAndContentBounds(t *testing.T) {
	const maximumContentBytes = 16 * 1024 * 1024
	large := strings.Repeat("x", maximumContentBytes+1)
	archive := snapshotTestArchive(
		t,
		[]snapshotArchiveEntry{
			{
				header: tar.Header{
					Name:     "fixture/large.go",
					Typeflag: tar.TypeReg,
					Size:     int64(len(large)),
					Mode:     0o600,
				},
				data: large,
			},
		},
	)
	client := snapshotTestClient(
		t,
		func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write(archive) },
	)
	repo := domain.Repository{Owner: "owner", Name: "repository"}
	head := domain.HeadSHA(strings.Repeat("a", 40))
	ctx, closeSnapshot, err := client.OpenRepositorySnapshot(t.Context(), 77, repo, head)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = closeSnapshot() })
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := client.GetFileContent(canceled, 77, repo, "large.go", head); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Fatalf("The repository snapshot did not preserve cancellation: %v", err)
	}

	if _, err := client.GetFileContent(ctx, 77, repo, "large.go", head); err == nil ||
		!strings.Contains(err.Error(), "content response byte limit") {
		t.Fatalf("Snapshot reads did not preserve the existing content byte limit: %v", err)
	}
}

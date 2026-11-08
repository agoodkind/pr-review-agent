package githubapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/testgit"
)

const (
	gitFixtureSourcePath     = "source.go"
	gitFixtureSourceContent  = "package fixture\nvar Value = 2\n"
	gitFixtureRepositoryName = "repository"
	gitFixtureTokenKey       = "token"
	gitFixtureExpiresAtKey   = "expires_at"
	gitFixtureNumberKey      = "number"
	gitFixtureHeadKey        = "head"
	gitFixtureSHAKey         = "sha"
	gitFixtureBaseKey        = "base"
	gitFixtureTitleKey       = "title"
	gitFixtureBodyKey        = "body"
	gitFixtureStateKey       = "state"
	gitFixtureOpenState      = "open"
	gitFixtureMergedKey      = "merged"
	gitFixtureDraftKey       = "draft"
)

type gitFixture struct {
	client           *githubapp.Client
	repo             domain.Repository
	root             domain.HeadSHA
	base             domain.HeadSHA
	head             domain.HeadSHA
	directory        string
	mutex            sync.Mutex
	transfers        int
	gitBytes         int64
	haves            []int
	contentRequests  int
	metadataRequests int
}

func gitCommand(t *testing.T, directory string, arguments ...string) string {
	t.Helper()

	return testgit.Command(t, directory, arguments...)
}

func writeGitFixtureFile(t *testing.T, directory, name, content string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func newGitFixtureRepository(t *testing.T) *gitFixture {
	t.Helper()
	projectRoot := t.TempDir()

	directory := filepath.Join(projectRoot, "owner", "repository.git")

	err := os.MkdirAll(directory, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	gitCommand(t, directory, "init", "--initial-branch=main")
	gitCommand(t, directory, "config", "uploadpack.allowAnySHA1InWant", "true")
	gitCommand(t, directory, "config", "commit.gpgsign", "false")
	writeGitFixtureFile(t, directory, gitFixtureSourcePath, "package fixture\nvar Value = 1\n")
	writeGitFixtureFile(t, directory, "old.go", "package fixture\nvar Old = true\n")
	writeGitFixtureFile(t,
		directory,
		"removed.go",
		"// This obsolete source has a distinct implementation.\nfunc Removed() { panic(\"obsolete\") }\n")
	gitCommand(t, directory, "add", ".")
	gitCommand(t, directory, "commit", "-m", "Add original sources")

	fixture := new(gitFixture)
	fixture.repo = domain.Repository{Owner: "owner", Name: gitFixtureRepositoryName}
	fixture.directory = directory
	fixture.root = domain.HeadSHA(gitCommand(t, directory, "rev-parse", "HEAD"))
	writeGitFixtureFile(t, directory, "base-only.go", "package fixture\nvar Base = true\n")
	gitCommand(t, directory, "add", ".")
	gitCommand(t, directory, "commit", "-m", "Add base branch file")
	fixture.base = domain.HeadSHA(gitCommand(t, directory, "rev-parse", "HEAD"))
	gitCommand(t, directory, "checkout", "-b", "feature", string(fixture.root))
	writeGitFixtureFile(t, directory, gitFixtureSourcePath, gitFixtureSourceContent)
	writeGitFixtureFile(t, directory, "added.go", "package fixture\nvar Added = true\n")
	writeGitFixtureFile(t, directory, ".gitattributes", "added.go linguist-generated=true\n")
	writeGitFixtureFile(t,
		directory,
		".gitmodules",
		"[submodule \"module\"]\npath = lib/module\nurl = https://example.invalid/nested.git\n")
	gitCommand(t, directory, "mv", "old.go", "renamed.go")
	gitCommand(t, directory, "rm", "removed.go")

	symlinkErr := os.Symlink(gitFixtureSourcePath, filepath.Join(directory, "link.go"))
	if symlinkErr != nil {
		t.Fatal(symlinkErr)
	}

	gitCommand(t, directory, "add", ".")
	gitCommand(t, directory, "update-index", "--add", "--cacheinfo", "160000,"+string(fixture.root)+",lib/module")
	gitCommand(t, directory, "commit", "-m", "Change feature sources")
	fixture.head = domain.HeadSHA(gitCommand(t, directory, "rev-parse", "HEAD"))

	return fixture
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	fixture := newGitFixtureRepository(t)
	directory := fixture.directory
	backend := testgit.Handler(t, directory)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.mutex.Lock()
		defer fixture.mutex.Unlock()

		if strings.HasPrefix(request.URL.Path, "/app/installations/") {
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]string{
				gitFixtureTokenKey:     "git-fixture-token",
				gitFixtureExpiresAtKey: time.Now().Add(time.Hour).Format(time.RFC3339),
			})
			return
		}

		if request.URL.Path == "/repos/owner/repository/pulls/7" {
			fixture.metadataRequests++
			_ = json.NewEncoder(writer).Encode(map[string]any{
				gitFixtureNumberKey: 7,
				gitFixtureHeadKey:   map[string]string{gitFixtureSHAKey: string(fixture.head)},
				gitFixtureBaseKey:   map[string]string{gitFixtureSHAKey: string(fixture.base)},
				gitFixtureTitleKey:  "Change fixture sources",
				gitFixtureBodyKey:   "",
				gitFixtureStateKey:  gitFixtureOpenState,
				gitFixtureMergedKey: false,
				gitFixtureDraftKey:  false,
			})
			return
		}

		if strings.HasPrefix(request.URL.Path, "/owner/repository.git/") {
			username, password, ok := request.BasicAuth()
			if !ok || username != "x-access-token" || password != "git-fixture-token" {
				t.Error("Git HTTPS did not use installation Basic authentication")
			}

			if request.Method == http.MethodPost {
				fixture.transfers++

				payload, err := io.ReadAll(request.Body)
				if err != nil {
					t.Error(err)

					return
				}

				fixture.haves = append(fixture.haves, bytes.Count(payload, []byte("have ")))
				request.Body = io.NopCloser(bytes.NewReader(payload))
			}

			backend.ServeHTTP(gitCountingWriter{ResponseWriter: writer, bytes: &fixture.gitBytes}, request)

			return
		}

		fixture.contentRequests++

		http.Error(writer, "Repository content APIs are forbidden", http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config

	cfg.GitHubAppID, cfg.GitHubPrivateKey, cfg.GitHubAPIBaseURL = 12345, testPrivateKey(t), address
	httpClient := testgit.Client(t, server)
	fixture.client = githubapp.NewClient(cfg, httpClient, time.Now, slog.New(slog.DiscardHandler))

	t.Cleanup(func() {
		err := fixture.client.Close()
		if err != nil {
			t.Error(err)
		}
	})

	return fixture
}

type gitCountingWriter struct {
	http.ResponseWriter

	bytes *int64
}

func (writer gitCountingWriter) Write(data []byte) (int, error) {
	count, err := writer.ResponseWriter.Write(data)
	*writer.bytes += int64(count)

	if err != nil {
		return count, fmt.Errorf("count Git response bytes: %w", err)
	}

	return count, nil
}

// TestGitTransportCollectsPinnedReviewWithoutContentAPIRequests verifies
// local collection and cached concurrent reads.
func TestGitTransportCollectsPinnedReviewWithoutContentAPIRequests(t *testing.T) {
	fixture := newGitFixture(t)
	client := fixture.client

	ctx, closeSnapshot, err := client.OpenRepositorySnapshot(t.Context(), 77, fixture.repo, fixture.head)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeSnapshot() })

	pullRequest, err := client.GetPullRequest(ctx, 77, fixture.repo, 7)
	if err != nil {
		t.Fatal(err)
	}

	ref := domain.PullRequestRef{InstallationID: 77, Repository: fixture.repo, Number: 7, Head: fixture.head}

	input, err := diff.NewCollector(client).Collect(ctx, ref, pullRequest)
	if err != nil {
		t.Fatal(err)
	}

	assertGitCollectedFiles(t, input)

	comparison, err := client.Compare(ctx, 77, fixture.repo, fixture.base, fixture.head)
	if err != nil || comparison.MergeBase != fixture.root {
		t.Fatalf("merge base = %s, want %s: %v", comparison.MergeBase, fixture.root, err)
	}

	assertGitSpecialFiles(t, fixture, ctx, comparison)

	paths, err := client.SearchRepository(ctx, 77, fixture.repo, "var Value = 2")
	if err != nil || !slices.Equal(paths, []string{gitFixtureSourcePath}) {
		t.Fatalf("literal search = %v: %v", paths, err)
	}

	_, foreignError := client.GetFileContent(ctx, 78, fixture.repo, gitFixtureSourcePath, fixture.head)
	if foreignError == nil {
		t.Fatal("A snapshot accepted another installation")
	}

	_, unsafePathError := client.GetFileContent(ctx, 77, fixture.repo, "../source.go", fixture.head)
	if unsafePathError == nil {
		t.Fatal("A snapshot accepted a parent path")
	}

	closeErr := closeSnapshot()
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	_, closedScopeError := client.GetFileContent(ctx, 77, fixture.repo, gitFixtureSourcePath, fixture.head)
	if closedScopeError == nil {
		t.Fatal("A closed snapshot accepted a read")
	}

	fixture.mutex.Lock()
	before := fixture.transfers
	fixture.mutex.Unlock()

	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			content, err := client.GetFileContent(t.Context(), 77, fixture.repo, gitFixtureSourcePath, fixture.head)
			if err != nil || string(content.Data) != gitFixtureSourceContent {
				t.Errorf("Repeated review = %q: %v", content.Data, err)
			}
		})
	}

	group.Wait()
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	if fixture.contentRequests != 0 || fixture.metadataRequests != 1 || fixture.transfers != before {
		t.Fatalf("content requests=%d, metadata requests=%d, transfers=%d, before repeated reviews=%d",
			fixture.contentRequests,
			fixture.metadataRequests,
			fixture.transfers,
			before)
	}

	t.Logf("content API requests=0; metadata requests=1; initial Git transfers=%d; repeated/concurrent Git transfers=0",
		before)
}

// TestGitTransportPreservesCancellation verifies cancellation before
// repository acquisition.
func TestGitTransportPreservesCancellation(t *testing.T) {
	fixture := newGitFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := fixture.client.GetFileContent(ctx, 77, fixture.repo, gitFixtureSourcePath, fixture.head)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func assertGitCollectedFiles(t *testing.T, input diff.ReviewInput) {
	t.Helper()

	paths := make([]string, 0, len(input.Files))
	for _, file := range input.Files {
		paths = append(paths, file.Path)
		if file.Path == gitFixtureSourcePath {
			if file.CurrentContent != gitFixtureSourceContent {
				t.Fatalf("source content = %q", file.CurrentContent)
			}

			if _, ok := file.ChangedRightLines[2]; !ok {
				t.Fatal("The collector did not preserve changed source line 2")
			}

			if _, ok := file.ChangedRightHunks[2]; !ok {
				t.Fatal("The collector did not retain the source hunk anchor")
			}
		}
	}

	want := []string{
		".gitattributes",
		".gitmodules",
		"added.go",
		"lib/module",
		"link.go",
		"removed.go",
		"renamed.go",
		gitFixtureSourcePath,
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("changed paths = %v, want %v", paths, want)
	}
}

func assertGitSpecialFiles(t *testing.T, fixture *gitFixture, ctx context.Context, comparison githubapp.Comparison) {
	t.Helper()

	client := fixture.client

	for _, file := range comparison.Files {
		if file.Path == "renamed.go" && (file.Status != "renamed" || file.PreviousPath != "old.go") {
			t.Fatalf("rename = %+v", file)
		}

		if file.Path == "removed.go" && (file.Status != "removed" || file.Deletions != 2) {
			t.Fatalf("deletion = %+v", file)
		}
	}

	link, err := client.GetFileContent(ctx, 77, fixture.repo, "link.go", fixture.head)
	if err != nil || string(link.Data) != gitFixtureSourcePath || !strings.Contains(link.Metadata,
		"Symbolic link target text") {
		t.Fatalf("symlink = %+v: %v", link, err)
	}

	module, err := client.GetFileContent(ctx, 77, fixture.repo, "lib/module", fixture.head)
	if err != nil ||
		string(module.Data) != "Subproject commit "+string(fixture.root)+"\n" ||
		module.Metadata != "Submodule repository: https://example.invalid/nested.git" {
		t.Fatalf("gitlink = %+v: %v", module, err)
	}
}

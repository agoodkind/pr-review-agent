// Package testgit serves temporary Git repositories through real smart HTTP.
package testgit

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cgi" // #nosec G504 -- Go removes the Proxy header before creating CGI variables.
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Command runs Git against a temporary fixture repository.
//
// Command fixes the author and commit time for repeatable commit identifiers.
//
// Example:
//
//	Command(t, directory, "status", "--short")
func Command(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	slog.Debug("Run temporary repository Git command")

	command := exec.CommandContext(t.Context(), "git", arguments...)
	command.Dir = directory

	command.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)

	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", arguments, output, err)
	}

	return strings.TrimSpace(string(output))
}

// New creates a private repository without hooks or external configuration.
//
// The test cleanup removes the repository after the test finishes.
//
// Example:
//
//	directory := New(t)
func New(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	Command(t, directory, "init", "--initial-branch=main", "--quiet")
	Command(t, directory, "config", "commit.gpgsign", "false")
	Command(t, directory, "config", "core.hooksPath", "/dev/null")

	return directory
}

// Commit saves the complete file and gitlink trees and returns the actual SHA.
//
// Commit replaces the previous tree and accepts an empty tree.
//
// Example:
//
//	head := Commit(t, directory, "Add source", files, gitlinks)
func Commit(t *testing.T, directory, subject string, files, gitlinks map[string]string) string {
	t.Helper()
	slog.Debug("Create temporary repository Git commit", "subject", subject)
	Command(t, directory, "rm", "-r", "--ignore-unmatch", ".")

	for name, content := range files {
		path := filepath.Join(directory, filepath.FromSlash(name))

		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(path, []byte(content), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	Command(t, directory, "add", "--all")

	for name, hash := range gitlinks {
		Command(t, directory, "update-index", "--add", "--cacheinfo", "160000,"+hash+","+name)
	}

	Command(t, directory, "commit", "--quiet", "--allow-empty", "-m", subject)

	return Command(t, directory, "rev-parse", "HEAD")
}

// Handler uses the installed Git server only inside test fixtures.
//
// Handler serves the supplied repository through native Git smart HTTP.
//
// Example:
//
//	gitHandler := Handler(t, directory)
func Handler(t *testing.T, directory string) http.Handler {
	t.Helper()
	Command(t, directory, "config", "uploadpack.allowAnySHA1InWant", "true")

	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}

	backend := &cgi.Handler{
		Path: gitBinary, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(directory), "GIT_HTTP_EXPORT_ALL=1"},
	}

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, suffix, found := strings.Cut(request.URL.Path, ".git/")
		if !found {
			http.Error(writer, "Git fixture path is invalid", http.StatusNotFound)

			return
		}

		rewritten := request.Clone(request.Context())
		address := *request.URL
		address.Path = "/" + filepath.Base(directory) + "/" + suffix
		rewritten.URL = &address
		backend.ServeHTTP(writer, rewritten)
	})
}

type fixtureTransport struct {
	target    *url.URL
	transport http.RoundTripper
}

func (transport fixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	rewritten := request.Clone(request.Context())
	address := *request.URL
	address.Scheme, address.Host = transport.target.Scheme, transport.target.Host
	rewritten.URL, rewritten.Host = &address, address.Host

	response, err := transport.transport.RoundTrip(rewritten)
	if response != nil {
		response.Request = request
	}

	if err != nil {
		slog.Warn("Request temporary Git server failed", "err", err)

		return nil, fmt.Errorf("request temporary Git server: %w", err)
	}

	return response, nil
}

// Client directs Git HTTPS requests to the local test server.
//
// Client preserves the original response request for Git authentication checks.
//
// Example:
//
//	httpClient := Client(t, server)
func Client(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()

	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	return &http.Client{Transport: fixtureTransport{target: address, transport: server.Client().Transport}}
}

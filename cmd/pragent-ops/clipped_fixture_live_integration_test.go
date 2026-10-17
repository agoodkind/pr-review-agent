//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

type clippedLivePR struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	State string `json:"state"`
	Draft bool   `json:"draft"`
	Head  struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

type clippedLiveFile struct {
	SHA      string `json:"sha"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

type clippedLiveFixture struct {
	opts     reviewBodyOptions
	client   *githubapp.Client
	repo     domain.Repository
	bot      string
	filePath string
	original clippedLivePR
	head     string
}

func clippedLiveSetup(t *testing.T) *clippedLiveFixture {
	t.Helper()
	opts := liveReviewBodyOptions(t)
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal("The dedicated fixture repository is invalid.")
	}
	cfg, err := reviewBodyIdentity(opts)
	if err != nil {
		t.Fatal("The GitHub App identity could not be loaded.")
	}
	filePath := os.Getenv("PR_AGENT_LIVE_CLIPPED_PATH")
	normalizedPath, pathErr := marker.NormalizePath(filePath)
	if pathErr != nil || normalizedPath != filePath || path.Ext(filePath) != ".md" {
		t.Fatal("The fixture requires a canonical repository-relative Markdown path.")
	}
	fixture := &clippedLiveFixture{opts: opts, repo: repo, bot: cfg.GitHubBotLogin, filePath: filePath, client: githubapp.NewClient(cfg, &http.Client{Timeout: 30 * time.Second}, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	fixture.original = fixture.pullRequest(t, t.Context())
	fixture.head = fixture.original.Head.SHA
	return fixture
}

func (fixture *clippedLiveFixture) pullRequest(t *testing.T, ctx context.Context) clippedLivePR {
	t.Helper()
	var result clippedLivePR
	clippedLiveAPI(t, ctx, http.MethodGet, fmt.Sprintf("repos/%s/pulls/%d", fixture.opts.repository, fixture.opts.pullRequest), nil, &result)
	if result.State != "open" || result.Draft || !strings.Contains(result.Title, "PR-Agent prose fixture") || result.Head.Repo.FullName != fixture.opts.repository || result.Head.Ref == "" || result.Head.SHA == "" {
		t.Fatal("The selected target must be an open, non-draft dedicated prose fixture in its own repository.")
	}
	return result
}

func (fixture *clippedLiveFixture) file(t *testing.T, ctx context.Context) clippedLiveFile {
	t.Helper()
	var result clippedLiveFile
	clippedLiveAPI(t, ctx, http.MethodGet, "repos/"+fixture.opts.repository+"/contents/"+fixture.filePath+"?ref="+url.QueryEscape(fixture.head), nil, &result)
	return result
}

func (fixture *clippedLiveFixture) write(t *testing.T, ctx context.Context, content, subject string) {
	t.Helper()
	current := fixture.pullRequest(t, ctx)
	if current.Head.SHA != fixture.head || current.Head.Ref != fixture.original.Head.Ref || current.Title != fixture.original.Title || current.Body != fixture.original.Body {
		t.Fatal("The fixture changed concurrently. The test will not overwrite another change.")
	}
	original := fixture.file(t, ctx)
	payload := struct {
		Message string `json:"message"`
		Content string `json:"content"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}{subject + "\n\nCo-authored-by: Codex <noreply@openai.com>", base64.StdEncoding.EncodeToString([]byte(content)), original.SHA, current.Head.Ref}
	var result struct {
		Commit struct {
			SHA     string `json:"sha"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		} `json:"commit"`
	}
	clippedLiveAPI(t, ctx, http.MethodPut, "repos/"+fixture.opts.repository+"/contents/"+fixture.filePath, payload, &result)
	if result.Commit.SHA == "" {
		t.Fatal("GitHub did not return the fixture commit identifier.")
	}
	previous := fixture.head
	fixture.head = result.Commit.SHA
	if len(result.Commit.Parents) != 1 || result.Commit.Parents[0].SHA != previous {
		t.Fatal("The fixture commit did not use the expected parent.")
	}
	var commit struct {
		Commit struct {
			Verification struct {
				Verified bool `json:"verified"`
			} `json:"verification"`
		} `json:"commit"`
	}
	clippedLiveAPI(t, ctx, http.MethodGet, "repos/"+fixture.opts.repository+"/commits/"+fixture.head, nil, &commit)
	if !commit.Commit.Verification.Verified {
		t.Fatal("GitHub did not verify the fixture commit signature.")
	}
	if fixture.pullRequest(t, ctx).Head.SHA != fixture.head {
		t.Fatal("The pull request head changed after the fixture commit.")
	}
	t.Logf("The fixture commit %s uses branch %s.", fixture.head, current.Head.Ref)
}

func clippedLivePassage(t *testing.T, name string) string {
	t.Helper()
	file := filepath.Join("..", "..", "testdata", name)
	data, err := os.ReadFile(file)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		t.Fatalf("The passage file for %s could not be read.", name)
	}
	return strings.TrimSpace(string(data))
}

func clippedLiveAssertAnchors(t *testing.T, threads []githubapp.ReviewThread, document, passage string) {
	t.Helper()
	seen := make(map[int]bool)
	for offset := 0; offset < len(document); {
		index := strings.Index(document[offset:], passage)
		if index < 0 {
			break
		}
		index += offset
		start := strings.Count(document[:index], "\n") + 1
		end := start + strings.Count(passage, "\n")
		for _, thread := range threads {
			if thread.RootComment.StartLine <= end && thread.RootComment.EndLine >= start {
				seen[start] = true
			}
		}
		offset = index + len(passage)
	}
	if len(seen) != len(threads) {
		t.Fatal("The inline findings did not identify each separate passage location.")
	}
}

func clippedLiveDecode(t *testing.T, file clippedLiveFile) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil || file.Encoding != "base64" || file.SHA == "" {
		t.Fatal("GitHub did not return a valid fixture file.")
	}
	return string(data)
}

func clippedLiveDocument(original, first, second, third string) string {
	result := original + "\n## First section\n\n" + first + "\n\n## Second section\n\n" + second + "\n"
	if third != "" {
		result += "\n## Third section\n\n" + third + "\n"
	}
	return result
}

func clippedLivePad(replacement, original string) string {
	difference := strings.Count(original, "\n") - strings.Count(replacement, "\n")
	if difference > 0 {
		replacement += strings.Repeat("\n", difference)
	}
	return replacement
}

func clippedLiveAPI(t *testing.T, ctx context.Context, method, endpoint string, input, result any) {
	t.Helper()
	arguments := []string{"api", "--method", method, endpoint}
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			t.Fatal("The fixture API request could not be encoded.")
		}
		arguments = append(arguments, "--input", "-")
	}
	command := exec.CommandContext(ctx, "gh", arguments...)
	command.Stdin = bytes.NewReader(body)
	var output, diagnostic bytes.Buffer
	command.Stdout = &output
	command.Stderr = &diagnostic
	if command.Run() != nil {
		t.Fatalf("The authenticated GitHub CLI failed during %s %s: %.512s", method, endpoint, diagnostic.String())
	}
	if json.Unmarshal(output.Bytes(), result) != nil {
		t.Fatal("GitHub returned an invalid fixture response.")
	}
}

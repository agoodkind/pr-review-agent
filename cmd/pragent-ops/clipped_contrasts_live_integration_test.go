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

	"goodkind.io/pr-review-agent/internal/diff"
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

func TestLiveClippedContrastsRemainBlockingAtEachLocation(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_CLIPPED_CONTRASTS_TEST") != "1" {
		t.Skip("The deployed clipped-contrast regression is not enabled.")
	}
	fixture := clippedLiveSetup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()
	originalFile := fixture.file(t, ctx)
	originalContent := clippedLiveDecode(t, originalFile)
	bad := clippedLivePassage(t, "clipped-contrast-bad.txt")
	fixed := clippedLivePassage(t, "clipped-contrast-corrected.txt")
	if bad == fixed || strings.Contains(originalContent, bad) {
		t.Fatal("The fixture requires distinct passages and an original file without the violation.")
	}
	if strings.Count(fixed, "\n") > strings.Count(bad, "\n") {
		t.Fatal("The replacement passage must fit within the original passage lines.")
	}
	baseline := fixture.await(t, ctx, "APPROVED", nil, 0)
	for _, thread := range baseline.Threads {
		if thread.RootComment.Author == fixture.bot && !thread.Resolved {
			t.Fatal("The dedicated fixture must not contain unresolved bot findings.")
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cleanupCancel()
		fixture.write(t, cleanupCtx, originalContent, "Restore prose fixture content")
		fixture.await(t, cleanupCtx, "APPROVED", nil, 0)
		current := fixture.pullRequest(t, cleanupCtx)
		if current.Title != fixture.original.Title || current.Body != fixture.original.Body || current.Draft != fixture.original.Draft || current.State != fixture.original.State {
			t.Error("The fixture metadata changed during the live regression.")
		}
		if clippedLiveDecode(t, fixture.file(t, cleanupCtx)) != originalContent {
			t.Error("The original fixture file did not survive cleanup.")
		}
	})
	first := clippedLiveDocument(originalContent, bad, bad, "")
	fixture.write(t, ctx, first, "Add repeated contrast passages to prose fixture")
	blocked := fixture.await(t, ctx, "CHANGES_REQUESTED", nil, 2)
	threads := clippedLiveOccurrences(t, blocked, fixture.bot, fixture.filePath, fixture.head)
	if len(threads) != 2 || threads[0].RootComment.StartLine == threads[1].RootComment.StartLine {
		t.Fatal("The deployed review did not identify both separate occurrences.")
	}
	clippedLiveAssertAnchors(t, threads, first, bad)
	firstID, secondID := threads[0].NodeID, threads[1].NodeID
	if threads[0].RootComment.StartLine > threads[1].RootComment.StartLine {
		firstID, secondID = secondID, firstID
	}
	fixture.write(t, ctx, clippedLiveDocument(originalContent, clippedLivePad(fixed, bad), bad, ""), "Correct the first contrast passage in prose fixture")
	fixture.await(t, ctx, "CHANGES_REQUESTED", map[string]bool{firstID: true, secondID: false}, 1)
	clean := clippedLiveDocument(originalContent, clippedLivePad(fixed, bad), clippedLivePad(fixed, bad), "")
	fixture.write(t, ctx, clean, "Correct the second contrast passage in prose fixture")
	fixture.await(t, ctx, "APPROVED", map[string]bool{firstID: true, secondID: true}, 0)
	third := clippedLiveDocument(originalContent, clippedLivePad(fixed, bad), clippedLivePad(fixed, bad), bad)
	fixture.write(t, ctx, third, "Add the contrast passage at another fixture location")
	reintroduced := fixture.await(t, ctx, "CHANGES_REQUESTED", map[string]bool{firstID: true, secondID: true}, 1)
	newThreads := clippedLiveOccurrences(t, reintroduced, fixture.bot, fixture.filePath, fixture.head)
	if len(newThreads) != 1 || newThreads[0].NodeID == firstID || newThreads[0].NodeID == secondID {
		t.Fatal("The new occurrence did not create its own blocking finding.")
	}
	clippedLiveAssertAnchors(t, newThreads, third, bad)
	t.Logf("The deployed service identified both occurrences, resolved each corrected thread, approved the corrected document, and blocked a new occurrence on %s#%d.", fixture.opts.repository, fixture.opts.pullRequest)
}

func TestLiveClippedContrastsRemainBlockingInDescription(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_CLIPPED_CONTRASTS_TEST") != "1" {
		t.Skip("The deployed clipped-contrast regression is not enabled.")
	}
	fixture := clippedLiveSetup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()
	bad := clippedLivePassage(t, "clipped-contrast-bad.txt")
	fixed := clippedLivePassage(t, "clipped-contrast-corrected.txt")
	if strings.Contains(fixture.original.Body, bad) || strings.Count(fixed, "\n") > strings.Count(bad, "\n") || bad == fixed {
		t.Fatal("The description fixture requires clean original prose and a distinct replacement within the original line count.")
	}
	fixture.awaitMetadata(t, ctx, fixture.original.Body, "APPROVED", 0)
	expectedBody := fixture.original.Body
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cleanupCancel()
		fixture.editBody(t, cleanupCtx, expectedBody, fixture.original.Body)
		fixture.awaitMetadata(t, cleanupCtx, fixture.original.Body, "APPROVED", 0)
	})
	duplicated := clippedLiveDocument(fixture.original.Body, bad, bad, "")
	fixture.editBody(t, ctx, expectedBody, duplicated)
	expectedBody = duplicated
	findings := fixture.awaitMetadata(t, ctx, expectedBody, "CHANGES_REQUESTED", 2)
	clippedLiveAssertMetadataAnchors(t, findings, expectedBody, bad)
	firstFixed := clippedLiveDocument(fixture.original.Body, clippedLivePad(fixed, bad), bad, "")
	fixture.editBody(t, ctx, expectedBody, firstFixed)
	expectedBody = firstFixed
	findings = fixture.awaitMetadata(t, ctx, expectedBody, "CHANGES_REQUESTED", 1)
	clippedLiveAssertMetadataAnchors(t, findings, expectedBody, bad)
	bothFixed := clippedLiveDocument(fixture.original.Body, clippedLivePad(fixed, bad), clippedLivePad(fixed, bad), "")
	fixture.editBody(t, ctx, expectedBody, bothFixed)
	expectedBody = bothFixed
	fixture.awaitMetadata(t, ctx, expectedBody, "APPROVED", 0)
	t.Logf("The deployed description review retained both occurrences, blocked the remaining occurrence after one correction, and approved both corrections on %s#%d.", fixture.opts.repository, fixture.opts.pullRequest)
}

func (fixture *clippedLiveFixture) editBody(t *testing.T, ctx context.Context, expected, replacement string) {
	t.Helper()
	current := fixture.pullRequest(t, ctx)
	if current.Head.SHA != fixture.head || current.Head.Ref != fixture.original.Head.Ref || current.Title != fixture.original.Title || current.Body != expected {
		t.Fatal("The fixture changed concurrently. The description edit will preserve the external change.")
	}
	if expected == replacement {
		return
	}
	var updated clippedLivePR
	clippedLiveAPI(t, ctx, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", fixture.opts.repository, fixture.opts.pullRequest), struct {
		Body string `json:"body"`
	}{replacement}, &updated)
	if updated.Head.SHA != fixture.head || updated.Head.Ref != fixture.original.Head.Ref || updated.Title != fixture.original.Title || updated.Body != replacement {
		t.Fatal("The updated description did not retain the expected head, branch, and title.")
	}
}

func (fixture *clippedLiveFixture) awaitMetadata(t *testing.T, parent context.Context, body, decision string, count int) []domain.Finding {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 6*time.Minute)
	defer cancel()
	last := "The deployed service has not completed the edited description."
	for ctx.Err() == nil {
		current := fixture.pullRequest(t, ctx)
		if current.Head.SHA != fixture.head || current.Title != fixture.original.Title || current.Body != body {
			t.Fatal("The fixture changed while waiting for the description review.")
		}
		snapshot := liveReviewBodyCapture(t, ctx, fixture.client, fixture.opts, fixture.repo)
		findings, revisionCurrent := clippedLiveMetadata(snapshot, fixture.bot)
		actual := githubapp.LatestDecisionReview(snapshot.Reviews, fixture.bot).State
		completed := validateReviewBodySummary(snapshot, fixture.bot) == nil
		next := fmt.Sprintf("The latest decision is %s. The summary completion is %t. The metadata revision match is %t. The summary contains %d metadata findings.", actual, completed, revisionCurrent, len(findings))
		if next != last {
			t.Log(next)
			last = next
		}
		if completed && revisionCurrent && actual == decision && len(findings) == count {
			return findings
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	t.Fatalf("The description review did not produce %s with %d metadata findings within six minutes. %s", decision, count, last)
	return nil
}

func clippedLiveMetadata(snapshot reviewBodySnapshot, bot string) ([]domain.Finding, bool) {
	const prefix = "<!-- pr-review-agent:metadata:v1 "
	for _, comment := range snapshot.Comments {
		if comment.Author != bot || !marker.HasState(comment.Body) {
			continue
		}
		index := strings.LastIndex(comment.Body, prefix)
		if index < 0 {
			continue
		}
		payload, _, found := strings.Cut(comment.Body[index+len(prefix):], " -->")
		decoded, err := base64.RawURLEncoding.DecodeString(payload)
		var record struct {
			Head     domain.HeadSHA   `json:"head"`
			Revision string           `json:"revision"`
			Findings []domain.Finding `json:"findings"`
		}
		if found && err == nil && json.Unmarshal(decoded, &record) == nil {
			return record.Findings, record.Head == snapshot.PullRequest.Head && record.Revision == diff.PullRequestRevision(snapshot.PullRequest)
		}
	}
	return nil, false
}

func clippedLiveAssertMetadataAnchors(t *testing.T, findings []domain.Finding, document, passage string) {
	t.Helper()
	var anchors []githubapp.ReviewThread
	for _, finding := range findings {
		if finding.EffectiveSurface() != domain.FindingPullRequestDescription || finding.RuleID != "clipped_contrasts" || finding.Importance != 10 {
			t.Fatal("The description findings must apply the clipped_contrasts rule with importance ten.")
		}
		anchors = append(anchors, githubapp.ReviewThread{RootComment: domain.ReviewComment{StartLine: finding.StartLine, EndLine: finding.EndLine}})
	}
	clippedLiveAssertAnchors(t, anchors, document, passage)
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

func (fixture *clippedLiveFixture) await(t *testing.T, ctx context.Context, decision string, resolved map[string]bool, openCount int) reviewBodySnapshot {
	t.Helper()
	for ctx.Err() == nil {
		if fixture.pullRequest(t, ctx).Head.SHA != fixture.head {
			t.Fatal("The fixture head changed while waiting for the deployed review.")
		}
		snapshot, err := captureReviewBodies(ctx, fixture.client, fixture.opts.installationID, fixture.repo, fixture.opts.pullRequest)
		if err != nil {
			t.Fatal("The deployed fixture review could not be read.")
		}
		if validateReviewBodySummary(snapshot, fixture.bot) == nil {
			actual := githubapp.LatestDecisionReview(snapshot.Reviews, fixture.bot).State
			open := 0
			seen := make(map[string]bool)
			matched := true
			for _, thread := range snapshot.Threads {
				if thread.RootComment.Author != fixture.bot {
					continue
				}
				if !thread.Resolved {
					open++
				}
				if want, exists := resolved[thread.NodeID]; exists {
					seen[thread.NodeID] = true
					matched = matched && want == thread.Resolved
				}
			}
			if actual == decision && open == openCount && matched && len(seen) == len(resolved) {
				return snapshot
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	t.Fatalf("The deployed review did not complete with decision %s and %d unresolved findings at %s.", decision, openCount, fixture.head)
	return reviewBodySnapshot{}
}

func clippedLiveOccurrences(t *testing.T, snapshot reviewBodySnapshot, bot, filePath, head string) []githubapp.ReviewThread {
	t.Helper()
	var result []githubapp.ReviewThread
	for _, thread := range snapshot.Threads {
		if thread.RootComment.Author != bot || thread.RootComment.Path != filePath || thread.Resolved {
			continue
		}
		finding, valid := marker.FindFinding(thread.RootComment.Body)
		if !valid || string(finding.Head) != head || finding.Importance != 10 {
			t.Fatal("The fixture occurrence must have a current-commit finding with importance ten.")
		}
		result = append(result, thread)
	}
	return result
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

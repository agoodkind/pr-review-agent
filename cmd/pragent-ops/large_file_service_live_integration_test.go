//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

const largeFileFixturePrefix = "docs/pr-agent-classification-fixture/"

var largeFileFixturePaths = []string{
	largeFileFixturePrefix + "generated.sql",
	largeFileFixturePrefix + "credentials.vault",
	largeFileFixturePrefix + "authorization.go",
	largeFileFixturePrefix + "sdk",
	".gitmodules",
}

type largeFileServiceFixture struct {
	fixture       *clippedLiveFixture
	worktree      string
	evidence      string
	originalTree  string
	currentBody   string
	origin        string
	revision      string
	limit         int
	originalForce bool
	minimumCheck  int64
	localHead     string
}

type largeFileServiceEvidence struct {
	Scenario string                    `json:"scenario"`
	Revision string                    `json:"service_revision"`
	Snapshot reviewBodySnapshot        `json:"snapshot"`
	Check    githubapp.CheckRunDetails `json:"check"`
}

type largeFileOriginalEvidence struct {
	Tree     string             `json:"tree"`
	Snapshot reviewBodySnapshot `json:"snapshot"`
}

type largeFileEvidence interface {
	largeFileOriginalEvidence | largeFileServiceEvidence | clippedLivePR
}

func TestLiveLargeFileClassificationsThroughDeployedService(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_LARGE_FILE_TEST") != "1" {
		t.Skip("Set PR_AGENT_LIVE_LARGE_FILE_TEST=1 to exercise the deployed classification service.")
	}
	live := largeFileServiceSetup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Minute)
	defer cancel()
	live.health(t, ctx)
	original := liveReviewBodyCapture(t, ctx, live.fixture.client, live.fixture.opts, live.fixture.repo)
	largeFileSave(t, live.evidence, "original.json", largeFileOriginalEvidence{Tree: live.originalTree, Snapshot: original})
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cleanupCancel()
		live.restore(t, cleanupCtx)
	})

	sqlPath := largeFileFixturePrefix + "generated.sql"
	sql := largeFileSQL(live.limit)
	live.stage(t, ctx, "Add generated migration review fixture", "The generated migration adds audit records. Existing classification_active_users records must remain intact. The fixture contains an unintended DROP TABLE statement that requires an inline finding. The test reviews the SQL without executing it.", map[string]string{sqlPath: sql}, "")
	sqlSources := map[domain.HeadSHA][]string{domain.HeadSHA(live.fixture.head): strings.Split(sql, "\n")}
	live.await(t, ctx, "generated_sql", func(snapshot reviewBodySnapshot, body string) bool {
		kind, action, found := largeFileAssessment(body, sqlPath)
		if !found || (kind != "generated" && kind != "source") || action != "review" {
			return false
		}
		if largeFileDetail(body, "All file content inspected") != "yes" || largeFileDetail(body, "Review scope assessed") != "yes" {
			return false
		}
		for _, label := range []string{"Files reviewed", "Diff chunks"} {
			count, err := strconv.Atoi(strings.Trim(largeFileDetail(body, label), "`"))
			if err != nil || count <= 0 {
				return false
			}
		}
		return live.openSQLFinding(t, ctx, snapshot, sqlPath, sql, sqlSources)
	})

	vaultPath := largeFileFixturePrefix + "credentials.vault"
	authorizationPath := largeFileFixturePrefix + "authorization.go"
	ciphertext := largeFileCiphertext(live.limit)
	rotation := "package fixture\n\n// The compiled role remains independent of the encrypted authentication token.\nconst CredentialSource = \"credentials.vault\"\nconst CredentialReference = \"service_token\"\n\nfunc AllowedRole(_ map[string]string) string { return \"reader\" }\n"
	live.stage(t, ctx, "Add encrypted credential rotation fixture", "Rotate the service_token authentication credential in credentials.vault. AllowedRole in authorization.go returns reader independently of the token. The test ciphertext does not contain real credentials. The fixture does not supply external validation results.", map[string]string{vaultPath: ciphertext, authorizationPath: rotation}, "")
	live.await(t, ctx, "credential_rotation", func(_ reviewBodySnapshot, body string) bool {
		_, action, found := largeFileAssessment(body, vaultPath)
		return found && action == "exclude" && largeFileDetail(body, "Review scope assessed") == "yes"
	})

	permissions := "package fixture\n\n// The encrypted document determines the role and allowed deletion resources.\nconst AuthorizationSource = \"credentials.vault\"\n\nfunc AllowedRole(vaultValues map[string]string) string { return vaultValues[\"role\"] }\n\nfunc DeletionResources(vaultValues map[string][]string) []string { return vaultValues[\"allowed_delete_resources\"] }\n"
	live.stage(t, ctx, "Add encrypted authorization review fixture", "Change role and allowed_delete_resources inside credentials.vault. authorization.go now delegates permission decisions to those encrypted values. The unchanged ciphertext is the same harmless test payload from the credential-rotation case. The fixture does not supply decrypted values or permission validation results.", map[string]string{vaultPath: ciphertext, authorizationPath: permissions}, "")
	live.await(t, ctx, "hidden_authorization", func(_ reviewBodySnapshot, body string) bool {
		_, action, found := largeFileAssessment(body, vaultPath)
		return found && action != "exclude" && largeFileDetail(body, "Review scope assessed") == "no"
	})

	first := live.fixture.original.Head.SHA
	second := clippedLiveGit(t, ctx, live.worktree, "rev-parse", first+"^")
	live.stage(t, ctx, "Add pinned parent integration fixture", "Add a fixture gitlink pinned to an existing commit in this repository. Review the parent pointer and .gitmodules integration. The fixture does not request nested upstream source review.", nil, first)
	live.await(t, ctx, "gitlink_initial", func(_ reviewBodySnapshot, body string) bool {
		return largeFileDetail(body, "All file content inspected") == "yes" && largeFileDetail(body, "Review scope assessed") == "yes"
	})
	live.stage(t, ctx, "Update pinned parent integration fixture", "Update the fixture gitlink to another existing commit in this repository. Review the changed parent pointer and .gitmodules integration. The fixture does not request nested upstream source review.", nil, second)
	live.await(t, ctx, "gitlink_updated", func(snapshot reviewBodySnapshot, body string) bool {
		if largeFileDetail(body, "All file content inspected") != "yes" || largeFileDetail(body, "Review scope assessed") != "yes" {
			return false
		}
		for _, thread := range snapshot.Threads {
			finding, valid := marker.FindFinding(thread.RootComment.Body)
			if valid && string(finding.Head) == live.fixture.head && thread.RootComment.Author == live.fixture.bot && !strings.HasPrefix(thread.RootComment.Path, largeFileFixturePrefix) && thread.RootComment.Path != ".gitmodules" && thread.RootComment.Path != live.fixture.filePath {
				return false
			}
		}
		return true
	})
}

func (live *largeFileServiceFixture) openSQLFinding(t *testing.T, ctx context.Context, snapshot reviewBodySnapshot, path, sql string, sources map[domain.HeadSHA][]string) bool {
	t.Helper()
	const statement = "DROP TABLE classification_active_users;"
	offset := strings.Index(sql, statement)
	if offset < 0 {
		t.Fatal("The SQL fixture must include its required DROP statement.")
	}
	line := strings.Count(sql[:offset], "\n") + 1
	currentStatement := strings.Split(sql, "\n")[line-1]
	for _, thread := range snapshot.Threads {
		comment := thread.RootComment
		finding, valid := marker.FindFinding(comment.Body)
		if !valid || thread.Resolved || comment.Author != live.fixture.bot || comment.Path != path || comment.StartLine > line || comment.EndLine < line {
			continue
		}
		source, cached := sources[finding.Head]
		if !cached {
			content, err := live.fixture.client.GetFile(ctx, live.fixture.opts.installationID, live.fixture.repo, path, finding.Head)
			if err != nil {
				t.Fatal("The immutable source for the existing SQL finding could not be verified.")
			}
			source = strings.Split(string(content), "\n")
			sources[finding.Head] = source
		}
		start, end := comment.StartLine, comment.EndLine
		if finding.OriginalStartLine > 0 {
			start, end = finding.OriginalStartLine, finding.OriginalEndLine
		}
		if start < 1 || end < start || end > len(source) {
			continue
		}
		for _, anchored := range source[start-1 : end] {
			if anchored == currentStatement {
				return true
			}
		}
	}
	return false
}

func largeFileServiceSetup(t *testing.T) *largeFileServiceFixture {
	t.Helper()
	fixture := clippedLiveSetup(t)
	worktree := os.Getenv("PR_AGENT_LIVE_CLIPPED_WORKTREE")
	if !filepath.IsAbs(worktree) || clippedLiveGit(t, t.Context(), worktree, "status", "--porcelain") != "" || clippedLiveGit(t, t.Context(), worktree, "rev-parse", "HEAD") != fixture.head || clippedLiveGit(t, t.Context(), worktree, "branch", "--show-current") != fixture.original.Head.Ref {
		t.Fatal("The dedicated fixture requires its clean, current, explicitly selected worktree.")
	}
	remote := clippedLiveGit(t, t.Context(), worktree, "remote", "get-url", "--push", "origin")
	if remote != "git@github.com:"+fixture.opts.repository+".git" && remote != "https://github.com/"+fixture.opts.repository+".git" {
		t.Fatal("The fixture worktree must push to its explicitly selected repository.")
	}
	if clippedLiveGit(t, t.Context(), worktree, "ls-tree", "-r", "HEAD", "--", largeFileFixturePrefix) != "" {
		t.Fatal("The classification fixture prefix already contains tracked work.")
	}
	if _, err := os.Lstat(filepath.Join(worktree, largeFileFixturePrefix)); !os.IsNotExist(err) {
		t.Fatal("The classification fixture prefix must not contain existing local files.")
	}
	if info, err := os.Lstat(filepath.Join(worktree, ".gitmodules")); err == nil && !info.Mode().IsRegular() {
		t.Fatal("The existing .gitmodules path must identify a regular file.")
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal("The existing .gitmodules path could not be inspected.")
	}
	revision := os.Getenv("PR_AGENT_LIVE_SERVICE_REVISION")
	if _, err := domain.ParseHeadSHA(revision); err != nil {
		t.Fatal("PR_AGENT_LIVE_SERVICE_REVISION must identify the expected deployed commit.")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var runtime struct {
		BudgetURL string `json:"PROVIDER_BUDGET_URL"`
		Limit     string `json:"REVIEW_MAX_PROMPT_BYTES"`
	}
	if json.Unmarshal(data, &runtime) != nil {
		t.Fatal("The public runtime configuration could not be decoded.")
	}
	endpoint, err := url.Parse(runtime.BudgetURL)
	limit, limitErr := strconv.Atoi(runtime.Limit)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || limitErr != nil || limit <= 0 {
		t.Fatal("The public runtime must configure a valid deployed origin and request limit.")
	}
	evidence, err := os.MkdirTemp("", "pragent-large-file-service-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Private classification acceptance evidence: %s", evidence)
	live := &largeFileServiceFixture{fixture: fixture, worktree: worktree, evidence: evidence, originalTree: clippedLiveGit(t, t.Context(), worktree, "rev-parse", "HEAD^{tree}"), currentBody: fixture.original.Body, origin: endpoint.Scheme + "://" + endpoint.Host, revision: revision, limit: limit, localHead: fixture.head}
	live.originalForce = live.forceLabelPresent(t, t.Context())
	return live
}

func (live *largeFileServiceFixture) stage(t *testing.T, ctx context.Context, subject, body string, files map[string]string, gitlink string) {
	t.Helper()
	live.guard(t, ctx)
	live.restorePaths(t, ctx)
	for path, content := range files {
		if !strings.HasPrefix(path, largeFileFixturePrefix) {
			t.Fatal("The scenario attempted to modify a path outside its declared fixture prefix.")
		}
		absolute := filepath.Join(live.worktree, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if gitlink != "" {
		modules := ""
		tracked := clippedLiveGit(t, ctx, live.worktree, "ls-files", "--", ".gitmodules")
		if tracked != "" {
			data, err := os.ReadFile(filepath.Join(live.worktree, ".gitmodules"))
			if err != nil {
				t.Fatal(err)
			}
			modules = string(data)
		}
		modules += "\n[submodule \"pr-agent-classification-fixture\"]\n\tpath = " + largeFileFixturePrefix + "sdk\n\turl = https://github.com/" + live.fixture.opts.repository + ".git\n"
		if err := os.WriteFile(filepath.Join(live.worktree, ".gitmodules"), []byte(modules), 0o600); err != nil {
			t.Fatal(err)
		}
		clippedLiveGit(t, ctx, live.worktree, "update-index", "--add", "--cacheinfo", "160000", gitlink, largeFileFixturePrefix+"sdk")
	}
	live.setBody(t, ctx, body)
	live.commit(t, ctx, subject)
	live.force(t, ctx)
}

func (live *largeFileServiceFixture) guard(t *testing.T, ctx context.Context) {
	t.Helper()
	current := live.fixture.pullRequest(t, ctx)
	if (current.Head.SHA != live.fixture.head && current.Head.SHA != live.localHead) || current.Head.Ref != live.fixture.original.Head.Ref || current.Title != live.fixture.original.Title || current.Body != live.currentBody || clippedLiveGit(t, ctx, live.worktree, "rev-parse", "HEAD") != live.localHead {
		t.Fatal("The fixture changed concurrently; the test will not overwrite that change.")
	}
	live.fixture.head = current.Head.SHA
	command := exec.CommandContext(ctx, "git", "status", "--porcelain", "-z", "--untracked-files=all")
	command.Dir = live.worktree
	status, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("The fixture worktree status could not be inspected: %.512s", status)
	}
	for _, entry := range strings.Split(string(status), "\x00") {
		if entry == "" {
			continue
		}
		if len(entry) < 4 || (!strings.HasPrefix(entry[3:], largeFileFixturePrefix) && entry[3:] != ".gitmodules") {
			t.Fatal("The fixture worktree contains unrelated work; the test will not overwrite it.")
		}
	}
}

func (live *largeFileServiceFixture) setBody(t *testing.T, ctx context.Context, body string) {
	t.Helper()
	var result clippedLivePR
	clippedLiveAPI(t, ctx, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", live.fixture.opts.repository, live.fixture.opts.pullRequest), struct {
		Body string `json:"body"`
	}{Body: body}, &result)
	if result.Body != body {
		t.Fatal("GitHub did not retain the scenario description.")
	}
	live.currentBody = body
}

func (live *largeFileServiceFixture) commit(t *testing.T, ctx context.Context, subject string) {
	t.Helper()
	var present []string
	for _, path := range largeFileFixturePaths {
		info, err := os.Lstat(filepath.Join(live.worktree, filepath.FromSlash(path)))
		if err == nil && info.Mode().IsRegular() {
			present = append(present, path)
		} else if err != nil && !os.IsNotExist(err) {
			t.Fatal("A declared fixture path could not be inspected for staging.")
		}
	}
	if len(present) > 0 {
		arguments := append([]string{"add", "--all", "--"}, present...)
		clippedLiveGit(t, ctx, live.worktree, arguments...)
	}
	if clippedLiveGit(t, ctx, live.worktree, "diff", "--cached", "--name-only") == "" {
		return
	}
	previous := live.fixture.head
	parent := live.localHead
	clippedLiveGit(t, ctx, live.worktree, "commit", "-S", "-m", subject+"\n\nCo-authored-by: Codex <noreply@openai.com>")
	live.localHead = clippedLiveGit(t, ctx, live.worktree, "rev-parse", "HEAD")
	clippedLiveGit(t, ctx, live.worktree, "push", "--force-with-lease=refs/heads/"+live.fixture.original.Head.Ref+":"+previous, "origin", "HEAD:refs/heads/"+live.fixture.original.Head.Ref)
	live.fixture.head = live.localHead
	live.fixture.awaitHead(t, ctx, previous)
	var committed struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Commit struct {
			Verification struct {
				Verified bool `json:"verified"`
			} `json:"verification"`
		} `json:"commit"`
	}
	clippedLiveAPI(t, ctx, http.MethodGet, "repos/"+live.fixture.opts.repository+"/commits/"+live.fixture.head, nil, &committed)
	if len(committed.Parents) != 1 || committed.Parents[0].SHA != parent || !committed.Commit.Verification.Verified {
		t.Fatal("The fixture commit must preserve its expected parent and a verified signature.")
	}
}

func (live *largeFileServiceFixture) restorePaths(t *testing.T, ctx context.Context) {
	t.Helper()
	arguments := append([]string{"ls-files", "--"}, largeFileFixturePaths...)
	tracked := strings.Fields(clippedLiveGit(t, ctx, live.worktree, arguments...))
	if len(tracked) > 0 {
		arguments = append([]string{"restore", "--source=" + live.fixture.original.Head.SHA, "--staged", "--worktree", "--"}, tracked...)
		clippedLiveGit(t, ctx, live.worktree, arguments...)
	}
	for _, path := range largeFileFixturePaths {
		if path == largeFileFixturePrefix+"sdk" {
			continue
		}
		if path == ".gitmodules" && clippedLiveGit(t, ctx, live.worktree, "ls-tree", live.fixture.original.Head.SHA, "--", path) != "" {
			continue
		}
		absolute := filepath.Join(live.worktree, filepath.FromSlash(path))
		if err := os.Remove(absolute); err != nil && !os.IsNotExist(err) {
			t.Fatal("An explicitly owned untracked fixture file could not be removed during restoration.")
		}
	}
	if err := os.Remove(filepath.Join(live.worktree, largeFileFixturePrefix)); err != nil && !os.IsNotExist(err) {
		t.Fatal("The explicitly owned fixture directory contains unexpected work and could not be removed during restoration.")
	}
}

func (live *largeFileServiceFixture) restore(t *testing.T, ctx context.Context) {
	t.Helper()
	live.guard(t, ctx)
	live.restorePaths(t, ctx)
	live.setBody(t, ctx, live.fixture.original.Body)
	live.commit(t, ctx, "Restore classification fixture content")
	if live.originalForce != live.forceLabelPresent(t, ctx) {
		live.setForceLabel(t, ctx, live.originalForce)
	}
	if clippedLiveGit(t, ctx, live.worktree, "rev-parse", "HEAD^{tree}") != live.originalTree || clippedLiveGit(t, ctx, live.worktree, "status", "--porcelain") != "" {
		t.Error("The fixture did not restore its exact original tree and clean worktree.")
	}
	current := live.fixture.pullRequest(t, ctx)
	if current.Title != live.fixture.original.Title || current.Body != live.fixture.original.Body || current.State != live.fixture.original.State || current.Draft != live.fixture.original.Draft {
		t.Error("The fixture did not restore its original pull request metadata.")
	}
	largeFileSave(t, live.evidence, "restored.json", current)
}

func (live *largeFileServiceFixture) forceLabelPresent(t *testing.T, ctx context.Context) bool {
	t.Helper()
	var labels []struct {
		Name string `json:"name"`
	}
	clippedLiveAPI(t, ctx, http.MethodGet, fmt.Sprintf("repos/%s/issues/%d/labels", live.fixture.opts.repository, live.fixture.opts.pullRequest), nil, &labels)
	for _, label := range labels {
		if label.Name == domain.RerunReviewLabel {
			return true
		}
	}
	return false
}

func (live *largeFileServiceFixture) setForceLabel(t *testing.T, ctx context.Context, present bool) {
	t.Helper()
	endpoint := fmt.Sprintf("repos/%s/issues/%d/labels", live.fixture.opts.repository, live.fixture.opts.pullRequest)
	if present {
		var result []struct {
			Name string `json:"name"`
		}
		clippedLiveAPI(t, ctx, http.MethodPost, endpoint, struct {
			Labels []string `json:"labels"`
		}{Labels: []string{domain.RerunReviewLabel}}, &result)
		return
	}
	command := exec.CommandContext(ctx, "gh", "api", "--method", http.MethodDelete, endpoint+"/"+url.PathEscape(domain.RerunReviewLabel))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("The fixture force label could not be restored: %.512s", output)
	}
}

func (live *largeFileServiceFixture) force(t *testing.T, ctx context.Context) {
	t.Helper()
	check, found, err := live.fixture.client.FindCheckRun(ctx, live.fixture.opts.installationID, live.fixture.repo, domain.HeadSHA(live.fixture.head), config.ReviewCheckName)
	if err != nil {
		t.Fatal("The prior fixture check could not be read before forcing full review.")
	}
	live.minimumCheck = 0
	if found {
		live.minimumCheck = check.ID
	}
	if live.forceLabelPresent(t, ctx) {
		live.setForceLabel(t, ctx, false)
	}
	live.setForceLabel(t, ctx, true)
}

func (live *largeFileServiceFixture) health(t *testing.T, ctx context.Context) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, live.origin+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: time.Minute}).Do(request)
	if err != nil {
		t.Fatal("The deployed review service could not confirm its revision.")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Pr-Agent-Revision") != live.revision {
		t.Fatal("The active review service does not match the explicitly expected healthy revision.")
	}
}

func (live *largeFileServiceFixture) await(t *testing.T, parent context.Context, scenario string, accepts func(reviewBodySnapshot, string) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 7*time.Minute)
	defer cancel()
	for ctx.Err() == nil {
		live.guard(t, ctx)
		snapshot := liveReviewBodyCapture(t, ctx, live.fixture.client, live.fixture.opts, live.fixture.repo)
		check, found, err := live.fixture.client.FindCheckRun(ctx, live.fixture.opts.installationID, live.fixture.repo, domain.HeadSHA(live.fixture.head), config.ReviewCheckName)
		if err != nil {
			t.Fatal("The deployed classification check could not be read.")
		}
		if found && check.Status == "completed" && check.ID > live.minimumCheck {
			for _, comment := range snapshot.Comments {
				state, valid := marker.DecodeState(comment.Body)
				head, marked := marker.FindReview(comment.Body)
				if comment.Author != live.fixture.bot || !valid || !marked || string(head) != live.fixture.head || state.RunID != check.ExternalID || largeFileDetail(comment.Body, "Forced run") != "yes" || !review.MetadataMatches(comment.Body, head, diff.PullRequestRevision(snapshot.PullRequest)) || !accepts(snapshot, comment.Body) {
					continue
				}
				requests, requestErr := strconv.Atoi(strings.Trim(largeFileDetail(comment.Body, "Requests"), "`"))
				if requestErr != nil || requests < 1 || largeFileDetail(comment.Body, "Model") == "unknown" {
					t.Fatal("The classification result did not report actual provider requests and a model.")
				}
				for _, label := range []string{"Requests reporting usage", "Input tokens", "Output tokens", "Total tokens"} {
					value, err := strconv.ParseInt(strings.Trim(largeFileDetail(comment.Body, label), "`"), 10, 64)
					if err != nil || value <= 0 {
						t.Fatalf("The completed classification review did not report positive %s.", label)
					}
				}
				details, err := live.fixture.client.GetCheckRunDetails(ctx, live.fixture.opts.installationID, live.fixture.repo, check.ID)
				if err != nil {
					t.Fatal("The completed classification check details could not be read.")
				}
				live.health(t, ctx)
				largeFileSave(t, live.evidence, scenario+".json", largeFileServiceEvidence{Scenario: scenario, Revision: live.revision, Snapshot: snapshot, Check: details})
				return
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	t.Fatalf("The deployed classification scenario %s did not establish its expected result on head %s.", scenario, live.fixture.head)
}

func largeFileSave[T largeFileEvidence](t *testing.T, directory, name string, value T) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil || os.WriteFile(filepath.Join(directory, name), data, 0o600) != nil {
		t.Fatal("The private classification evidence could not be saved.")
	}
}

func largeFileAssessment(body, path string) (kind, action string, found bool) {
	for line := range strings.SplitSeq(body, "\n") {
		columns := strings.Split(line, "|")
		if len(columns) >= 6 && strings.TrimSpace(columns[1]) == path {
			return strings.TrimSpace(columns[2]), strings.TrimSpace(columns[3]), true
		}
	}
	return "", "", false
}

func largeFileDetail(body, name string) string {
	for line := range strings.SplitSeq(body, "\n") {
		columns := strings.Split(line, "|")
		if len(columns) == 4 && strings.TrimSpace(columns[1]) == name {
			return strings.TrimSpace(columns[2])
		}
	}
	return ""
}

func largeFileSQL(minimum int) string {
	var source strings.Builder
	source.WriteString("-- Code generated by the harmless classification fixture generator; DO NOT EDIT.\n-- Preserve existing classification_active_users records throughout this migration.\nCREATE TABLE classification_audit (id integer PRIMARY KEY, note text);\n")
	for index := 0; source.Len() <= minimum; index++ {
		fmt.Fprintf(&source, "INSERT INTO classification_audit VALUES (%d, 'fixture record %d');\n", index, index)
	}
	source.WriteString("DROP TABLE classification_active_users;\n")
	return source.String()
}

func largeFileCiphertext(minimum int) string {
	const row = "6439656266616532393062646262633330323265323061336534306662636663\n"
	return "$ANSIBLE_VAULT;1.1;AES256\n" + strings.Repeat(row, minimum/len(row)+2)
}

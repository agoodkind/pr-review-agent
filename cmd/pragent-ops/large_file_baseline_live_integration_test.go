//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
)

type largeFileBaselineSpec struct {
	Files     map[string]string
	Gitlink   string
	Modified  []string
	Oversized string
}

type largeFileBaselineRecord struct {
	Scenario string            `json:"scenario"`
	Ref      string            `json:"ref"`
	Commit   string            `json:"commit"`
	Parent   string            `json:"parent"`
	Tree     string            `json:"tree"`
	Digests  map[string]string `json:"file_digests"`
	Gitlink  string            `json:"gitlink,omitempty"`
	Removed  bool              `json:"removed"`
}

type largeFileBaselineRecovery struct {
	Original      clippedLivePR             `json:"original"`
	OriginalTree  string                    `json:"original_tree"`
	OriginalForce bool                      `json:"original_force_label"`
	RemoteHead    string                    `json:"expected_remote_head"`
	LocalHead     string                    `json:"local_head"`
	CurrentBase   clippedLiveBase           `json:"current_base"`
	CurrentDraft  bool                      `json:"current_draft"`
	Baselines     []largeFileBaselineRecord `json:"baselines"`
	OwnedCommits  []string                  `json:"owned_commits"`
}

func (live *largeFileServiceFixture) pullRequest(t *testing.T, ctx context.Context) clippedLivePR {
	t.Helper()
	var response struct {
		clippedLivePR
		DraftValue *bool   `json:"draft"`
		BodyValue  *string `json:"body"`
	}
	clippedLiveAPI(t, ctx, http.MethodGet, fmt.Sprintf("repos/%s/pulls/%d", live.fixture.opts.repository, live.fixture.opts.pullRequest), nil, &response)
	current := response.clippedLivePR
	if response.DraftValue == nil || response.BodyValue == nil {
		t.Fatal("The fixture response must include its draft state and body.")
	}
	current.Draft, current.Body = *response.DraftValue, *response.BodyValue
	_, headErr := domain.ParseHeadSHA(current.Head.SHA)
	_, baseErr := domain.ParseHeadSHA(current.Base.SHA)
	if current.State != "open" || !strings.Contains(current.Title, "PR-Agent prose fixture") || current.Head.Repo.FullName != live.fixture.opts.repository || current.Base.Repo.FullName != live.fixture.opts.repository || current.Head.Ref == "" || current.Base.Ref == "" || headErr != nil || baseErr != nil {
		t.Fatal("The dedicated fixture requires complete current head, base, repository, and open-state metadata.")
	}
	return current
}

func (live *largeFileServiceFixture) saveBaselineRecovery(t *testing.T) {
	t.Helper()
	largeFileSave(t, live.evidence, "baseline-recovery.json", largeFileBaselineRecovery{
		Original: live.fixture.original, OriginalTree: live.originalTree, OriginalForce: live.originalForce,
		RemoteHead: live.fixture.head, LocalHead: live.localHead, CurrentBase: live.currentBase, CurrentDraft: live.currentDraft,
		Baselines: live.baselines, OwnedCommits: live.ownedCommits,
	})
}

func (live *largeFileServiceFixture) setDraft(t *testing.T, ctx context.Context, draft bool) {
	t.Helper()
	live.guard(t, ctx)
	if live.currentDraft == draft {
		return
	}
	arguments := []string{"pr", "ready", fmt.Sprint(live.fixture.opts.pullRequest), "--repo", live.fixture.opts.repository}
	if draft {
		arguments = append(arguments, "--undo")
	}
	output, err := exec.CommandContext(ctx, "gh", arguments...).CombinedOutput()
	if err != nil {
		t.Fatalf("The fixture draft transition failed: %.512s", output)
	}
	current := live.pullRequest(t, ctx)
	if current.Draft != draft || current.Head.SHA != live.fixture.head || current.Base != live.currentBase || current.Title != live.fixture.original.Title || current.Body != live.currentBody {
		t.Fatal("The fixture draft transition did not preserve its expected head, base, and metadata.")
	}
	live.currentDraft = draft
	live.saveBaselineRecovery(t)
}

func (live *largeFileServiceFixture) setBase(t *testing.T, ctx context.Context, base clippedLiveBase) {
	t.Helper()
	live.guard(t, ctx)
	if !live.currentDraft {
		t.Fatal("The fixture must be draft before its comparison base changes.")
	}
	if live.currentBase == base {
		return
	}
	if base.Ref == "" || base.SHA == "" || base.Repo.FullName != live.fixture.opts.repository {
		t.Fatal("The fixture base requires an exact branch, commit, and repository.")
	}
	var updated clippedLivePR
	clippedLiveAPI(t, ctx, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", live.fixture.opts.repository, live.fixture.opts.pullRequest), struct {
		Base string `json:"base"`
	}{Base: base.Ref}, &updated)
	current := live.pullRequest(t, ctx)
	if current.Base != base || !current.Draft || current.Head.SHA != live.fixture.head || current.Title != live.fixture.original.Title || current.Body != live.currentBody {
		t.Fatal("GitHub did not retain the expected draft comparison base and fixture metadata.")
	}
	live.currentBase = base
	live.saveBaselineRecovery(t)
}

func (live *largeFileServiceFixture) awaitHead(t *testing.T, parent context.Context, previous string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		current := live.pullRequest(t, ctx)
		if current.Base != live.currentBase || current.Draft != live.currentDraft || current.Title != live.fixture.original.Title || current.Body != live.currentBody {
			t.Fatal("The fixture metadata changed while its signed head was being published.")
		}
		if current.Head.SHA == live.fixture.head {
			return
		}
		if current.Head.SHA != previous {
			t.Fatal("An unrelated commit changed the fixture head after its signed push.")
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	t.Fatalf("GitHub did not report the signed fixture commit: %v", ctx.Err())
}

func (live *largeFileServiceFixture) beginScenario(t *testing.T, ctx context.Context) {
	t.Helper()
	live.setDraft(t, ctx, true)
	live.setBase(t, ctx, live.fixture.original.Base)
}

func (live *largeFileServiceFixture) stageWithBaseline(t *testing.T, ctx context.Context, scenario, subject, body string, spec largeFileBaselineSpec, files map[string]string, gitlink string) {
	t.Helper()
	live.beginScenario(t, ctx)
	live.restorePaths(t, ctx)
	live.writeFiles(t, ctx, spec.Files, spec.Gitlink)
	parent := live.localHead
	if !live.commitLocal(t, ctx, "Create "+scenario+" fixture baseline", true) {
		t.Fatal("The scenario baseline must change its explicitly owned fixture content.")
	}
	baseline := largeFileBaselineRecord{
		Scenario: scenario, Ref: "refs/heads/codex/pragent-live-baseline-" + filepath.Base(live.evidence) + "-" + scenario,
		Commit: live.localHead, Parent: parent, Tree: clippedLiveGit(t, ctx, live.worktree, "rev-parse", "HEAD^{tree}"),
		Digests: make(map[string]string), Gitlink: spec.Gitlink,
	}
	for path, content := range spec.Files {
		digest := sha256.Sum256([]byte(content))
		baseline.Digests[path] = hex.EncodeToString(digest[:])
	}
	if spec.Gitlink != "" {
		content, err := os.ReadFile(filepath.Join(live.worktree, ".gitmodules"))
		if err != nil {
			t.Fatal("The generated baseline integration file could not be read.")
		}
		digest := sha256.Sum256(content)
		baseline.Digests[".gitmodules"] = hex.EncodeToString(digest[:])
	}
	live.baselines = append(live.baselines, baseline)
	live.saveBaselineRecovery(t)
	live.verifyRegenerableBaseline(t, ctx, baseline)
	clippedLiveGit(t, ctx, live.worktree, "push", "--force-with-lease="+baseline.Ref+":", "origin", baseline.Commit+":"+baseline.Ref)
	live.verifyCommit(t, ctx, baseline.Commit, parent)
	live.restorePaths(t, ctx)
	live.writeFiles(t, ctx, files, gitlink)
	live.setBody(t, ctx, body)
	live.commit(t, ctx, subject)
	base := live.fixture.original.Base
	base.Ref, base.SHA = strings.TrimPrefix(baseline.Ref, "refs/heads/"), baseline.Commit
	live.setBase(t, ctx, base)
	live.verifyBaselineComparison(t, ctx, baseline, spec)
	live.setDraft(t, ctx, false)
	live.force(t, ctx)
}

func (live *largeFileServiceFixture) verifyBaselineComparison(t *testing.T, ctx context.Context, baseline largeFileBaselineRecord, spec largeFileBaselineSpec) {
	t.Helper()
	var comparison struct {
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
		Files []struct {
			Path   string `json:"filename"`
			Status string `json:"status"`
		} `json:"files"`
	}
	endpoint := "repos/" + live.fixture.opts.repository + "/compare/" + baseline.Commit + "..." + live.fixture.head
	clippedLiveAPI(t, ctx, http.MethodGet, endpoint, nil, &comparison)
	if comparison.MergeBase.SHA != baseline.Commit || len(comparison.Files) != len(spec.Modified) || clippedLiveGit(t, ctx, live.worktree, "rev-parse", "HEAD^") != baseline.Commit {
		t.Fatal("The scenario must compare its direct baseline parent without unrelated file changes.")
	}
	for _, file := range comparison.Files {
		if file.Status != "modified" || !slices.Contains(spec.Modified, file.Path) {
			t.Fatal("GitHub did not report the required existing file as modified against its owned baseline.")
		}
	}
	if spec.Oversized != "" {
		patch := clippedLiveGit(t, ctx, live.worktree, "diff", "--no-ext-diff", "--no-renames", "--unified=3", baseline.Commit, live.fixture.head, "--", spec.Oversized)
		if len(patch) <= live.limit {
			t.Fatal("The actual credential change must exceed the configured request limit.")
		}
	}
}

func (live *largeFileServiceFixture) verifyCommit(t *testing.T, ctx context.Context, commit, parent string) {
	t.Helper()
	var recorded struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Commit struct {
			Verification struct {
				Verified bool `json:"verified"`
			} `json:"verification"`
		} `json:"commit"`
	}
	clippedLiveAPI(t, ctx, http.MethodGet, "repos/"+live.fixture.opts.repository+"/commits/"+commit, nil, &recorded)
	if len(recorded.Parents) != 1 || recorded.Parents[0].SHA != parent || !recorded.Commit.Verification.Verified {
		t.Fatal("The fixture commit must preserve its expected parent and a verified signature.")
	}
}

func (live *largeFileServiceFixture) removeBaselineRefs(t *testing.T, ctx context.Context) {
	t.Helper()
	if len(live.baselines) == 0 {
		return
	}
	current := live.pullRequest(t, ctx)
	if current.Head.SHA != live.fixture.head || current.Head.Ref != live.fixture.original.Head.Ref || current.Base != live.fixture.original.Base || current.Draft != live.fixture.original.Draft || current.Title != live.fixture.original.Title || current.Body != live.fixture.original.Body || live.forceLabelPresent(t, ctx) != live.originalForce || clippedLiveGit(t, ctx, live.worktree, "rev-parse", "HEAD^{tree}") != live.originalTree || clippedLiveGit(t, ctx, live.worktree, "status", "--porcelain") != "" {
		t.Fatal("Generated baseline refs require complete fixture restoration before cleanup.")
	}
	clippedLiveGit(t, ctx, live.worktree, "fetch", "origin", "--prune", "+refs/heads/*:refs/remotes/origin/*")
	for index := range live.baselines {
		baseline := &live.baselines[index]
		if baseline.Removed {
			continue
		}
		live.verifyRegenerableBaseline(t, ctx, *baseline)
		ref := strings.TrimPrefix(baseline.Ref, "refs/heads/")
		for _, filter := range []string{"base=" + url.QueryEscape(ref), "head=" + url.QueryEscape(live.fixture.repo.Owner+":"+ref)} {
			var open []struct {
				Number int `json:"number"`
			}
			clippedLiveAPI(t, ctx, http.MethodGet, "repos/"+live.fixture.opts.repository+"/pulls?state=open&"+filter, nil, &open)
			if len(open) != 0 {
				t.Fatal("An open pull request still uses a generated baseline ref; private recovery evidence retains its identity.")
			}
		}
		remote := clippedLiveGit(t, ctx, live.worktree, "ls-remote", "--heads", "origin", baseline.Ref)
		if remote != "" {
			fields := strings.Fields(remote)
			if len(fields) != 2 || fields[0] != baseline.Commit || fields[1] != baseline.Ref {
				t.Fatal("A generated baseline ref changed after creation; cleanup will preserve it.")
			}
			clippedLiveGit(t, ctx, live.worktree, "push", "--force-with-lease="+baseline.Ref+":"+baseline.Commit, "origin", ":"+baseline.Ref)
		}
		if clippedLiveGit(t, ctx, live.worktree, "ls-remote", "--heads", "origin", baseline.Ref) != "" {
			t.Fatal("The generated baseline ref still exists after leased cleanup.")
		}
		baseline.Removed = true
		live.saveBaselineRecovery(t)
	}
	clippedLiveGit(t, ctx, live.worktree, "fetch", "origin", "--prune", "+refs/heads/*:refs/remotes/origin/*")
}

func (live *largeFileServiceFixture) verifyRegenerableBaseline(t *testing.T, ctx context.Context, baseline largeFileBaselineRecord) {
	t.Helper()
	// These refs contain generated fixture commits, not production changes.
	// Verify every commit against this run's recorded generator output before removal.
	if !strings.HasPrefix(baseline.Ref, "refs/heads/codex/pragent-live-baseline-"+filepath.Base(live.evidence)+"-") || baseline.Ref == "refs/heads/"+live.fixture.original.Base.Ref || baseline.Ref == "refs/heads/"+live.fixture.original.Head.Ref || clippedLiveGit(t, ctx, live.worktree, "rev-parse", baseline.Commit+"^{tree}") != baseline.Tree || clippedLiveGit(t, ctx, live.worktree, "rev-parse", baseline.Commit+"^") != baseline.Parent {
		t.Fatal("The baseline ref does not match this run's recorded regenerable fixture tree.")
	}
	commits := strings.Fields(clippedLiveGit(t, ctx, live.worktree, "rev-list", live.fixture.original.Head.SHA+".."+baseline.Commit))
	for _, commit := range commits {
		if !slices.Contains(live.ownedCommits, commit) {
			t.Fatal("The generated baseline contains an unrecorded commit; cleanup will preserve it.")
		}
	}
	for path, expected := range baseline.Digests {
		command := exec.CommandContext(ctx, "git", "show", baseline.Commit+":"+path)
		command.Dir = live.worktree
		content, err := command.Output()
		if err != nil {
			t.Fatalf("Git could not read the recorded generated baseline file: %v", err)
		}
		digest := sha256.Sum256(content)
		if hex.EncodeToString(digest[:]) != expected {
			t.Fatal("A baseline file does not match the recorded generated input.")
		}
	}
	expectedPaths := make([]string, 0, len(baseline.Digests)+1)
	for path := range baseline.Digests {
		expectedPaths = append(expectedPaths, path)
	}
	if baseline.Gitlink != "" {
		expectedPaths = append(expectedPaths, largeFileFixturePrefix+"sdk")
	}
	changedPaths := strings.Fields(clippedLiveGit(t, ctx, live.worktree, "diff", "--name-only", live.fixture.original.Head.SHA, baseline.Commit, "--"))
	if len(changedPaths) != len(expectedPaths) {
		t.Fatal("The generated baseline contains changes outside its recorded fixture inputs.")
	}
	for _, path := range changedPaths {
		if !slices.Contains(expectedPaths, path) {
			t.Fatal("The generated baseline contains an unrelated changed path.")
		}
	}
	if baseline.Gitlink != "" {
		entry := strings.Fields(clippedLiveGit(t, ctx, live.worktree, "ls-tree", baseline.Commit, "--", largeFileFixturePrefix+"sdk"))
		if len(entry) != 4 || entry[0] != "160000" || entry[2] != baseline.Gitlink {
			t.Fatal("The generated baseline gitlink does not match its recorded pointer.")
		}
	}
	if clippedLiveGit(t, ctx, live.worktree, "for-each-ref", "--format=%(refname)", baseline.Ref) != "" {
		t.Fatal("An unexpected local branch uses the generated baseline name; cleanup will preserve it.")
	}
}

func (live *largeFileServiceFixture) commitLocal(t *testing.T, ctx context.Context, subject string, allowEmpty bool) bool {
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
		clippedLiveGit(t, ctx, live.worktree, append([]string{"add", "--all", "--"}, present...)...)
	}
	if clippedLiveGit(t, ctx, live.worktree, "diff", "--cached", "--name-only") == "" && !allowEmpty {
		return false
	}
	arguments := []string{"commit", "-S", "-m", subject + "\n\nCo-authored-by: Codex <noreply@openai.com>"}
	if allowEmpty {
		arguments = append(arguments, "--allow-empty")
	}
	clippedLiveGit(t, ctx, live.worktree, arguments...)
	live.localHead = clippedLiveGit(t, ctx, live.worktree, "rev-parse", "HEAD")
	live.ownedCommits = append(live.ownedCommits, live.localHead)
	live.saveBaselineRecovery(t)
	return true
}

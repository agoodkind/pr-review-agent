package diff_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// repositorySource reads immutable Git objects and exposes incomplete patch representations.
//
//testdouble:external GitHub revision and file queries execute against real temporary Git repositories.
type repositorySource struct {
	directory    string
	base         domain.HeadSHA
	head         domain.HeadSHA
	patchMode    string
	comparisons  int
	contentReads []repositoryContentRead
}

type repositoryContentRead struct {
	path     string
	revision domain.HeadSHA
}

func TestCollectorRecoversIncompleteGitChangesAtTheMergeBase(t *testing.T) {
	for _, mode := range []string{"absent", "malformed", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			source, mergeBase, expected := newRecoveryRepository(t, mode)
			input, err := diff.NewCollector(source).Collect(t.Context(), recoveryRef(source.head), recoveryPullRequest(source))
			if err != nil {
				t.Fatal(err)
			}
			if input.MergeBase != mergeBase || input.MergeBase == source.base {
				t.Fatal("Recovery must use the common ancestor rather than the newer base tip.")
			}
			if source.comparisons != 1 {
				t.Fatalf("Recovery resolved the comparison base %d times; expected one resolution for all files.", source.comparisons)
			}
			assertRecoveredRepositoryFiles(t, source, input, expected)
			foundOldRename := false
			for _, read := range source.contentReads {
				if read.path == "original.txt" && read.revision == mergeBase {
					foundOldRename = true
				}
				if read.revision != mergeBase && read.revision != source.head {
					t.Fatal("Content retrieval used a revision outside the pinned comparison.")
				}
			}
			if mode != "truncated" && !foundOldRename {
				t.Fatal("Recovery did not retrieve the renamed file's previous path at the merge base.")
			}
		})
	}
}

func TestCollectorRangeReusesItsActualGitComparisonBase(t *testing.T) {
	source, mergeBase, expected := newRecoveryRepository(t, "absent")
	input, err := diff.NewCollector(source).CollectRange(t.Context(), recoveryRef(source.head), recoveryPullRequest(source), mergeBase)
	if err != nil {
		t.Fatal(err)
	}
	if input.MergeBase != mergeBase || source.comparisons != 1 {
		t.Fatal("Range recovery must reuse its existing pinned comparison.")
	}
	assertRecoveredRepositoryFiles(t, source, input, expected)
}

func TestCollectorDoesNotRecoverBinaryGitObjectsAsText(t *testing.T) {
	source, _, _ := newRecoveryRepository(t, "absent")
	writeRecoveryFile(t, source.directory, "binary.bin", "header\x00payload\xff")
	largeGitCommand(t, source.directory, "add", "--all")
	largeGitCommit(t, source.directory, "Add binary content")
	source.head = domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, source.directory, "rev-parse", "HEAD")))
	input, err := diff.NewCollector(source).Collect(t.Context(), recoveryRef(source.head), recoveryPullRequest(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range input.Files {
		if file.Path == "binary.bin" {
			if file.CoverageComplete || file.Gap != diff.CoverageGapBinary || file.Patch != "" {
				t.Fatal("Binary object bytes must remain a visible gap without a fabricated text patch.")
			}
			return
		}
	}
	t.Fatal("Collection did not preserve the binary file's coverage record.")
}

func TestCollectorPreservesGitlinkCoordinatesAndRepositoryContext(t *testing.T) {
	for _, mode := range []string{"complete", "absent"} {
		t.Run(mode, func(t *testing.T) {
			library := t.TempDir()
			largeGitCommand(t, library, "init", "--quiet")
			writeRecoveryFile(t, library, "library.txt", "first version\n")
			largeGitCommand(t, library, "add", "--all")
			largeGitCommit(t, library, "Create library source")
			first := strings.TrimSpace(largeGitCommand(t, library, "rev-parse", "HEAD"))
			writeRecoveryFile(t, library, "library.txt", "second version\n")
			largeGitCommand(t, library, "add", "--all")
			largeGitCommit(t, library, "Update library source")
			second := strings.TrimSpace(largeGitCommand(t, library, "rev-parse", "HEAD"))
			directory := t.TempDir()
			largeGitCommand(t, directory, "init", "--quiet")
			const repositoryURL = "https://example.invalid/library.git"
			writeRecoveryFile(t, directory, ".gitmodules", "[submodule \"lib\"]\n\tpath = lib\n\turl = "+repositoryURL+"\n")
			largeGitCommand(t, directory, "add", "--", ".gitmodules")
			largeGitCommand(t, directory, "update-index", "--add", "--cacheinfo", "160000", first, "lib")
			largeGitCommit(t, directory, "Pin library source")
			base := domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", "HEAD")))
			largeGitCommand(t, directory, "update-index", "--cacheinfo", "160000", second, "lib")
			largeGitCommit(t, directory, "Update pinned library source")
			head := domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", "HEAD")))
			source := &repositorySource{directory: directory, base: base, head: head, patchMode: mode, comparisons: 0, contentReads: nil}
			input, err := diff.NewCollector(source).Collect(t.Context(), recoveryRef(head), recoveryPullRequest(source))
			if err != nil {
				t.Fatal(err)
			}
			if len(input.Files) != 1 || input.Files[0].Path != "lib" || !input.Files[0].CoverageComplete {
				t.Fatal("The actual gitlink change must remain reviewable.")
			}
			raw := "Subproject commit " + second + "\n"
			if input.Files[0].CurrentContent != raw {
				t.Fatal("Repository metadata changed the raw gitlink's line coordinates.")
			}
			chunks, err := diff.ChunkInput(diff.ReviewInput{Files: input.Files}, 80000)
			if err != nil {
				t.Fatal(err)
			}
			if len(chunks) != 1 {
				t.Fatal("The gitlink change must fit one review request.")
			}
			for _, evidence := range []string{first, second, repositoryURL} {
				if !strings.Contains(chunks[0].Text, evidence) {
					t.Fatalf("The gitlink review does not include %s.", evidence)
				}
			}
			rows, err := diff.ChangedSourceRows(chunks[0])
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Path != "lib" || rows[0].RightLine != 1 || rows[0].Text != strings.TrimSuffix(raw, "\n") {
				t.Fatal("The gitlink finding must anchor to the actual changed source at line 1.")
			}
			if err := diff.ValidatePatchContent(input.Files[0].Patch, raw); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func assertRecoveredRepositoryFiles(t *testing.T, source *repositorySource, input diff.ReviewInput, expected map[string]map[int]string) {
	t.Helper()
	if len(input.Files) != len(expected) {
		t.Fatalf("Recovered %d files; expected %d.", len(input.Files), len(expected))
	}
	for _, file := range input.Files {
		if !file.CoverageComplete || file.Gap != diff.CoverageGapNone {
			t.Fatalf("The recoverable text file %s remains incomplete.", file.Path)
		}
		if file.Status != "removed" {
			actual := largeGitCommand(t, source.directory, "show", string(source.head)+":"+file.Path)
			if file.CurrentContent != actual {
				t.Fatalf("Recovered current content does not match the immutable head for %s.", file.Path)
			}
			if err := diff.ValidatePatchContent(file.Patch, actual); err != nil {
				t.Fatalf("Recovered patch does not match head content for %s: %v", file.Path, err)
			}
		}
		chunks, err := diff.ChunkInput(diff.ReviewInput{Files: []diff.FileContext{file}}, 80000)
		if err != nil {
			t.Fatal(err)
		}
		actualRows := make(map[int]string)
		for _, chunk := range chunks {
			rows, err := diff.ChangedSourceRows(chunk)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				actualRows[row.RightLine] = row.Text
				if !diff.ValidRange(file.ChangedRightLines, file.ChangedRightHunks, row.RightLine, row.RightLine) {
					t.Fatal("A recovered changed row cannot anchor an inline finding.")
				}
			}
		}
		if !reflect.DeepEqual(actualRows, expected[file.Path]) {
			t.Fatalf("Recovered anchors do not match the actual changes for %s: %v", file.Path, actualRows)
		}
		if len(file.ChangedRightLines) != len(actualRows) {
			t.Fatal("Recovery made unchanged lines eligible for inline findings.")
		}
	}
}

func newRecoveryRepository(t *testing.T, mode string) (*repositorySource, domain.HeadSHA, map[string]map[int]string) {
	t.Helper()
	directory := t.TempDir()
	largeGitCommand(t, directory, "init", "--quiet")
	lines := make([]string, 80)
	for index := range lines {
		lines[index] = "unchanged source line " + strconv.Itoa(index+1)
	}
	baseContent := strings.Join(lines, "\n") + "\n"
	writeRecoveryFile(t, directory, "modified.txt", baseContent)
	writeRecoveryFile(t, directory, "original.txt", baseContent)
	writeRecoveryFile(t, directory, "removed.txt", "remove first\nremove last")
	largeGitCommand(t, directory, "add", "--all")
	largeGitCommit(t, directory, "Create recovery source")
	mergeBase := domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", "HEAD")))
	lines[4], lines[70] = "first café change", "later 雪 change"
	writeRecoveryFile(t, directory, "modified.txt", strings.Join(lines, "\n")+"\n")
	if err := os.Rename(filepath.Join(directory, "original.txt"), filepath.Join(directory, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	renameLines := strings.Split(strings.TrimSuffix(baseContent, "\n"), "\n")
	renameLines[39] = "renamed source change"
	writeRecoveryFile(t, directory, "renamed.txt", strings.Join(renameLines, "\n")+"\n")
	writeRecoveryFile(t, directory, "added.txt", "added café\nadded 雪")
	if err := os.Remove(filepath.Join(directory, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	largeGitCommand(t, directory, "add", "--all")
	largeGitCommit(t, directory, "Update recovery source")
	head := domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", "HEAD")))
	writeRecoveryFile(t, directory, "modified.txt", "base branch only\n"+baseContent)
	largeGitCommand(t, directory, "add", "--all")
	tree := strings.TrimSpace(largeGitCommand(t, directory, "write-tree"))
	base := domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, directory, "-c", "user.name=Integration Test", "-c", "user.email=integration@example.invalid", "commit-tree", tree, "-p", string(mergeBase), "-m", "Create divergent base tip\n\nCo-authored-by: Codex <noreply@openai.com>")))
	source := &repositorySource{directory: directory, base: base, head: head, patchMode: mode, comparisons: 0, contentReads: nil}
	expected := map[string]map[int]string{
		"modified.txt": {5: "first café change", 71: "later 雪 change"},
		"added.txt":    {1: "added café", 2: "added 雪"},
		"renamed.txt":  {40: "renamed source change"},
		"removed.txt":  {},
	}
	return source, mergeBase, expected
}

func writeRecoveryFile(t *testing.T, directory, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func recoveryRef(head domain.HeadSHA) domain.PullRequestRef {
	return domain.PullRequestRef{Repository: domain.Repository{Owner: "local", Name: "source"}, Number: 1, InstallationID: 1, Head: head}
}

func recoveryPullRequest(source *repositorySource) githubapp.PullRequest {
	return githubapp.PullRequest{Number: 1, Base: source.base, Head: source.head, Title: "Review repository changes", Body: "Review changed text at immutable commits."}
}

func (source *repositorySource) command(ctx context.Context, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = source.directory
	return command.Output()
}

func (source *repositorySource) ListChangedFiles(ctx context.Context, _ int64, _ domain.Repository, _ int) ([]githubapp.ChangedFile, error) {
	comparison, err := source.compare(ctx, source.base, source.head)
	return comparison.Files, err
}

func (source *repositorySource) Compare(ctx context.Context, _ int64, _ domain.Repository, base, head domain.HeadSHA) (githubapp.Comparison, error) {
	source.comparisons++
	return source.compare(ctx, base, head)
}

func (source *repositorySource) compare(ctx context.Context, base, head domain.HeadSHA) (githubapp.Comparison, error) {
	ancestor, err := source.command(ctx, "merge-base", string(base), string(head))
	if err != nil {
		return githubapp.Comparison{}, err
	}
	mergeBase := domain.HeadSHA(strings.TrimSpace(string(ancestor)))
	paths, err := source.command(ctx, "diff", "--name-status", "--find-renames", string(mergeBase), string(head))
	if err != nil {
		return githubapp.Comparison{}, err
	}
	comparison := githubapp.Comparison{MergeBase: mergeBase, Files: nil}
	for _, entry := range strings.Split(strings.TrimSpace(string(paths)), "\n") {
		fields := strings.Split(entry, "\t")
		if len(fields) < 2 {
			return githubapp.Comparison{}, errors.New("Git did not return a changed path")
		}
		file, err := source.changedFile(ctx, mergeBase, head, fields)
		if err != nil {
			return githubapp.Comparison{}, err
		}
		comparison.Files = append(comparison.Files, file)
	}
	return comparison, nil
}

func (source *repositorySource) changedFile(ctx context.Context, base, head domain.HeadSHA, fields []string) (githubapp.ChangedFile, error) {
	file := githubapp.ChangedFile{Path: fields[1], PreviousPath: "", Status: "modified", Patch: "", PatchPresent: true, Additions: 0, Deletions: 0, ChangeCountsKnown: false}
	switch fields[0][0] {
	case 'A':
		file.Status = "added"
	case 'D':
		file.Status = "removed"
	case 'R':
		file.Status, file.PreviousPath, file.Path = "renamed", fields[1], fields[2]
	}
	arguments := []string{"diff", "--find-renames", string(base), string(head), "--", file.Path}
	if file.PreviousPath != "" {
		arguments = append(arguments, file.PreviousPath)
	}
	patch, err := source.command(ctx, arguments...)
	if err != nil {
		return githubapp.ChangedFile{}, err
	}
	counts, err := source.command(ctx, append([]string{"diff", "--numstat", "--find-renames", string(base), string(head), "--"}, arguments[5:]...)...)
	if err != nil {
		return githubapp.ChangedFile{}, err
	}
	numbers := strings.Split(string(counts), "\t")
	if len(numbers) < 2 {
		return githubapp.ChangedFile{}, errors.New("Git did not return change counts")
	}
	file.Additions, err = strconv.Atoi(numbers[0])
	if err == nil {
		file.Deletions, err = strconv.Atoi(numbers[1])
		file.ChangeCountsKnown = err == nil
	}
	file.Patch = string(patch)
	switch source.patchMode {
	case "absent":
		file.Patch, file.PatchPresent = "", false
	case "malformed":
		start := strings.Index(file.Patch, "@@")
		if start >= 0 {
			file.Patch = file.Patch[:start+2]
		}
	case "truncated":
		start := strings.Index(file.Patch, "@@")
		if start >= 0 {
			end := strings.Index(file.Patch[start+2:], "\n@@")
			if end >= 0 {
				file.Patch = file.Patch[:start+2+end]
			}
		}
	}
	return file, nil
}

func (source *repositorySource) GetFile(ctx context.Context, installationID int64, repo domain.Repository, path string, revision domain.HeadSHA) ([]byte, error) {
	file, err := source.GetFileContent(ctx, installationID, repo, path, revision)
	return file.Data, err
}

func (source *repositorySource) GetFileContent(ctx context.Context, _ int64, _ domain.Repository, path string, revision domain.HeadSHA) (githubapp.FileContent, error) {
	source.contentReads = append(source.contentReads, repositoryContentRead{path: path, revision: revision})
	entry, err := source.command(ctx, "ls-tree", string(revision), "--", path)
	if err != nil {
		return githubapp.FileContent{}, err
	}
	fields := strings.Fields(string(entry))
	if len(fields) < 3 {
		if len(fields) == 0 {
			return githubapp.FileContent{}, githubapp.APIError{StatusCode: http.StatusNotFound, Message: "pinned file does not exist"}
		}
		return githubapp.FileContent{}, errors.New("Git did not return the pinned file entry")
	}
	if fields[0] == "160000" {
		repositoryURL, err := source.command(ctx, "config", "--blob", string(revision)+":.gitmodules", "--get", "submodule."+path+".url")
		if err != nil {
			return githubapp.FileContent{}, err
		}
		return githubapp.FileContent{Data: fmt.Appendf(nil, "Subproject commit %s\n", fields[2]), Metadata: "Submodule repository: " + strings.TrimSpace(string(repositoryURL)) + "\n"}, nil
	}
	content, err := source.command(ctx, "show", string(revision)+":"+path)
	return githubapp.FileContent{Data: content, Metadata: ""}, err
}

func (source *repositorySource) ListPullRequestCommits(ctx context.Context, _ int64, _ domain.Repository, _ int) ([]githubapp.PullRequestCommit, error) {
	output, err := source.command(ctx, "rev-list", string(source.base)+".."+string(source.head))
	if err != nil {
		return nil, err
	}
	var commits []githubapp.PullRequestCommit
	for _, revision := range strings.Fields(string(output)) {
		message, err := source.command(ctx, "show", "-s", "--format=%B", revision)
		if err != nil {
			return nil, err
		}
		commits = append(commits, githubapp.PullRequestCommit{SHA: domain.HeadSHA(revision), Message: string(message)})
	}
	return commits, nil
}

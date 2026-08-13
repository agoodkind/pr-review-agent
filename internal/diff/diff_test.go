package diff_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

const testHeadSHA = "a3c4f1cac7f595bc824704b9d2a1f1191630dc32"

func TestChangedRightLinesMultipleHunks(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/a.go b/a.go",
		"--- a/a.go",
		"+++ b/a.go",
		"@@ -1,3 +1,4 @@",
		" package a",
		"+added1",
		" func main() {}",
		"@@ -8,2 +9,3 @@",
		" func other() {}",
		"+added9",
	}, "\n")

	changed, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatalf("ChangedRightLines: %v", err)
	}
	if !containsLine(changed, 2) {
		t.Fatalf("changed lines = %v, want line 2", changed)
	}
	if !containsLine(changed, 10) {
		t.Fatalf("changed lines = %v, want line 10", changed)
	}
	if containsLine(changed, 1) {
		t.Fatalf("context line 1 should not be changed: %v", changed)
	}
}

func TestChangedRightLinesDeletionAndContextExcluded(t *testing.T) {
	patch := strings.Join([]string{
		"@@ -4,4 +4,3 @@",
		" context",
		"-deleted",
		" context2",
	}, "\n")

	changed, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatalf("ChangedRightLines: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("deleted-only hunk changed lines = %v, want empty", changed)
	}
}

func TestChangedRightLinesMalformedHeader(t *testing.T) {
	_, err := diff.ChangedRightLines("@@ not-a-hunk @@\n context")
	if err == nil {
		t.Fatal("malformed header: want error")
	}
}

func TestChangedRightLinesTruncatedHunk(t *testing.T) {
	patch := strings.Join([]string{
		"@@ -1,3 +1,4 @@",
		" context",
		"+added",
	}, "\n")

	changed, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatalf("ChangedRightLines: %v", err)
	}
	if !containsLine(changed, 2) {
		t.Fatalf("changed lines = %v, want line 2", changed)
	}

	input := diff.ReviewInput{
		Files: []diff.FileContext{{
			Path:              "a.go",
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    "context\nadded\ncontext2\n",
			ChangedRightLines: changed,
			CoverageComplete:  false,
		}},
	}
	chunks, err := diff.ChunkInput(input, 10_000)
	if err != nil {
		t.Fatalf("ChunkInput: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunk count = %d, want 1", len(chunks))
	}
	if chunks[0].CoverageComplete {
		t.Fatal("truncated hunk chunk should be incomplete")
	}
}

func TestValidRangeSingleAndMultilineWithinHunk(t *testing.T) {
	patch := strings.Join([]string{
		"@@ -1,5 +1,7 @@",
		" context1",
		"+added2",
		"+added3",
		"+added4",
		" context5",
	}, "\n")

	changed, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatalf("ChangedRightLines: %v", err)
	}
	if !diff.ValidRange(changed, 2, 2) {
		t.Fatal("single added line should be valid")
	}
	if !diff.ValidRange(changed, 2, 4) {
		t.Fatal("contiguous added lines in one hunk should be valid")
	}
	if diff.ValidRange(changed, 2, 5) {
		t.Fatal("range including context end line should be invalid")
	}
}

func TestValidRangeRejectsLinesAcrossHunks(t *testing.T) {
	patch := strings.Join([]string{
		"@@ -1,2 +1,3 @@",
		" ctx",
		"+line2",
		"@@ -3,1 +4,2 @@",
		"+line4",
	}, "\n")

	changed, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatalf("ChangedRightLines: %v", err)
	}
	if diff.ValidRange(changed, 2, 4) {
		t.Fatal("range across hunks should be invalid")
	}
}

func TestCollectorModifiedFile(t *testing.T) {
	source := &fakeSource{
		files: []githubapp.ChangedFile{{
			Path:         "pkg/a.go",
			Status:       "modified",
			Patch:        "@@ -1,1 +1,2 @@\n line\n+added\n",
			PatchPresent: true,
		}},
		contents: map[string][]byte{
			"pkg/a.go": []byte("line\nadded\n"),
		},
	}
	collector := diff.NewCollector(source)
	input, err := collector.Collect(context.Background(), testRef(), testPullRequest())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(input.Files) != 1 {
		t.Fatalf("file count = %d, want 1", len(input.Files))
	}
	file := input.Files[0]
	if !file.CoverageComplete {
		t.Fatal("modified file with patch and content should be complete")
	}
	if !containsLine(file.ChangedRightLines, 2) {
		t.Fatalf("changed lines = %v, want line 2", file.ChangedRightLines)
	}
	if file.CurrentContent != "line\nadded\n" {
		t.Fatalf("content = %q", file.CurrentContent)
	}
}

func TestCollectorRenamedFile(t *testing.T) {
	source := &fakeSource{
		files: []githubapp.ChangedFile{{
			Path:         "pkg/new.go",
			PreviousPath: "pkg/old.go",
			Status:       "renamed",
			Patch:        "@@ -1,1 +1,2 @@\n line\n+added\n",
			PatchPresent: true,
		}},
		contents: map[string][]byte{
			"pkg/new.go": []byte("line\nadded\n"),
		},
	}
	input, err := diff.NewCollector(source).Collect(context.Background(), testRef(), testPullRequest())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if input.Files[0].Path != "pkg/new.go" {
		t.Fatalf("path = %q, want pkg/new.go", input.Files[0].Path)
	}
	if !input.Files[0].CoverageComplete {
		t.Fatal("renamed file should be complete")
	}
}

func TestCollectorDeletedFileSkipsContent(t *testing.T) {
	source := &fakeSource{
		files: []githubapp.ChangedFile{{
			Path:         "pkg/removed.go",
			Status:       "removed",
			Patch:        "@@ -1,2 +0,0 @@\n-line1\n-line2\n",
			PatchPresent: true,
		}},
	}
	input, err := diff.NewCollector(source).Collect(context.Background(), testRef(), testPullRequest())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	file := input.Files[0]
	if file.CurrentContent != "" {
		t.Fatalf("deleted file content = %q, want empty", file.CurrentContent)
	}
	if source.getCalls != 0 {
		t.Fatalf("GetFile calls = %d, want 0", source.getCalls)
	}
	if !file.CoverageComplete {
		t.Fatal("deleted file with valid patch should be complete")
	}
}

func TestCollectorBinaryFileIncomplete(t *testing.T) {
	source := &fakeSource{
		files: []githubapp.ChangedFile{{
			Path:         "image.png",
			Status:       "added",
			PatchPresent: false,
		}},
	}
	input, err := diff.NewCollector(source).Collect(context.Background(), testRef(), testPullRequest())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if input.Files[0].CoverageComplete {
		t.Fatal("binary file without patch should be incomplete")
	}
}

func TestCollectorMissingPatchIncomplete(t *testing.T) {
	source := &fakeSource{
		files: []githubapp.ChangedFile{{
			Path:         "big.bin",
			Status:       "modified",
			PatchPresent: false,
		}},
	}
	input, err := diff.NewCollector(source).Collect(context.Background(), testRef(), testPullRequest())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if input.Files[0].CoverageComplete {
		t.Fatal("missing patch should be incomplete")
	}
}

func TestCollectorFailedContentReadIncomplete(t *testing.T) {
	source := &fakeSource{
		files: []githubapp.ChangedFile{{
			Path:         "pkg/a.go",
			Status:       "modified",
			Patch:        "@@ -1,1 +1,2 @@\n line\n+added\n",
			PatchPresent: true,
		}},
		getErr: errors.New("read failed"),
	}
	input, err := diff.NewCollector(source).Collect(context.Background(), testRef(), testPullRequest())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if input.Files[0].CoverageComplete {
		t.Fatal("failed content read should be incomplete")
	}
}

func TestCollectorDeterministicPathOrder(t *testing.T) {
	source := &fakeSource{
		files: []githubapp.ChangedFile{
			{Path: "z.go", Status: "modified", Patch: "@@ -1 +1 @@\n z\n", PatchPresent: true},
			{Path: "a.go", Status: "modified", Patch: "@@ -1 +1 @@\n a\n", PatchPresent: true},
			{Path: "m.go", Status: "modified", Patch: "@@ -1 +1 @@\n m\n", PatchPresent: true},
		},
		contents: map[string][]byte{
			"z.go": []byte("z\n"),
			"a.go": []byte("a\n"),
			"m.go": []byte("m\n"),
		},
	}
	input, err := diff.NewCollector(source).Collect(context.Background(), testRef(), testPullRequest())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	paths := make([]string, 0, len(input.Files))
	for _, file := range input.Files {
		paths = append(paths, file.Path)
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("paths = %v, want sorted", paths)
	}
}

func TestChunkInputEachHunkAppearsOnce(t *testing.T) {
	patchOne := "@@ -1,1 +1,2 @@\n a\n+one\n"
	patchTwo := "@@ -4,1 +5,2 @@\n d\n+two\n"
	input := diff.ReviewInput{
		Files: []diff.FileContext{{
			Path:             "a.go",
			Status:           "modified",
			Patch:            patchOne + "\n" + patchTwo,
			CurrentContent:   "a\none\nb\nc\nd\ntwo\n",
			CoverageComplete: true,
		}},
	}

	chunks, err := diff.ChunkInput(input, 10_000)
	if err != nil {
		t.Fatalf("ChunkInput: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunk count = %d, want 1", len(chunks))
	}
	if strings.Count(chunks[0].Text, "Diff hunk:") != 2 {
		t.Fatalf("chunk text hunk count = %d, want 2", strings.Count(chunks[0].Text, "Diff hunk:"))
	}
	if chunks[0].Index != 1 || chunks[0].Total != 1 {
		t.Fatalf("chunk index/total = %d/%d, want 1/1", chunks[0].Index, chunks[0].Total)
	}
}

func TestChunkInputSplitsBetweenHunks(t *testing.T) {
	patchOne := "@@ -1,1 +1,2 @@\n a\n+one\n"
	patchTwo := "@@ -4,1 +5,2 @@\n d\n+two\n"
	input := diff.ReviewInput{
		Files: []diff.FileContext{{
			Path:             "a.go",
			Status:           "modified",
			Patch:            patchOne + "\n" + patchTwo,
			CurrentContent:   "a\none\nb\nc\nd\ntwo\n",
			CoverageComplete: true,
		}},
	}

	chunks, err := diff.ChunkInput(input, 120)
	if err != nil {
		t.Fatalf("ChunkInput: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunk count = %d, want 2", len(chunks))
	}
	if strings.Count(chunks[0].Text, "Diff hunk:") != 1 {
		t.Fatal("first chunk should contain one hunk")
	}
	if strings.Count(chunks[1].Text, "Diff hunk:") != 1 {
		t.Fatal("second chunk should contain one hunk")
	}
}

func TestChunkInputOversizedHunkIncomplete(t *testing.T) {
	input := diff.ReviewInput{
		Files: []diff.FileContext{{
			Path:             "a.go",
			Status:           "modified",
			Patch:            "@@ -1,1 +1,2 @@\n a\n+added\n",
			CurrentContent:   strings.Repeat("x", 200),
			CoverageComplete: true,
		}},
	}

	chunks, err := diff.ChunkInput(input, 80)
	if err != nil {
		t.Fatalf("ChunkInput: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunk count = %d, want 1", len(chunks))
	}
	if chunks[0].CoverageComplete {
		t.Fatal("oversized hunk should force incomplete coverage")
	}
	if !strings.Contains(chunks[0].Text, "coverage incomplete") {
		t.Fatalf("chunk text = %q, want incomplete metadata", chunks[0].Text)
	}
}

type fakeSource struct {
	files    []githubapp.ChangedFile
	contents map[string][]byte
	getErr   error
	getCalls int
}

func (source *fakeSource) ListChangedFiles(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	_ int,
) ([]githubapp.ChangedFile, error) {
	return source.files, nil
}

func (source *fakeSource) GetFile(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	path string,
	_ domain.HeadSHA,
) ([]byte, error) {
	source.getCalls++
	if source.getErr != nil {
		return nil, source.getErr
	}
	content, ok := source.contents[path]
	if !ok {
		return nil, errors.New("missing content")
	}
	return content, nil
}

func testRef() domain.PullRequestRef {
	head, err := domain.ParseHeadSHA(testHeadSHA)
	if err != nil {
		panic(err)
	}
	return domain.PullRequestRef{
		Repository:     domain.Repository{Owner: "owner", Name: "repo"},
		Number:         1,
		InstallationID: 99,
		Head:           head,
	}
}

func testPullRequest() githubapp.PullRequest {
	head, err := domain.ParseHeadSHA(testHeadSHA)
	if err != nil {
		panic(err)
	}
	return githubapp.PullRequest{
		Number: 1,
		Head:   head,
		Title:  "title",
		Body:   "body",
	}
}

func containsLine(changed map[int]struct{}, line int) bool {
	_, ok := changed[line]
	return ok
}

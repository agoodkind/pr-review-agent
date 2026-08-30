package diff_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// A line's new position decides which code a reconciliation reads, so each case
// here names the shift it is about rather than checking arithmetic in the
// abstract.
func TestMapLineToNewSideMovesLinesByWhatThePatchDid(t *testing.T) {
	// One line inserted after old line 5, consuming no old line.
	const insertion = "@@ -5,0 +6,1 @@\n+inserted\n"
	// Three lines inserted above the anchor, which is then rewritten.
	const rewrite = "@@ -1,3 +1,5 @@\n line1\n+a\n+b\n+c\n-issue\n+fixed\n line3\n"

	cases := []struct {
		name    string
		patch   string
		oldLine int
		want    int
		mapped  bool
	}{
		{
			// The line the insertion is anchored after is untouched by it. Counting
			// it as covered pushed it onto the inserted text instead.
			name:  "line a pure insertion is anchored after keeps its place",
			patch: insertion, oldLine: 5, want: 5, mapped: true,
		},
		{
			name:  "line after a pure insertion moves by what it added",
			patch: insertion, oldLine: 6, want: 7, mapped: true,
		},
		{
			name:  "line before a pure insertion is unmoved",
			patch: insertion, oldLine: 4, want: 4, mapped: true,
		},
		{
			name:  "context line before an edit is unmoved",
			patch: rewrite, oldLine: 1, want: 1, mapped: true,
		},
		{
			name:  "replaced line lands where its replacement sits",
			patch: rewrite, oldLine: 2, want: 5, mapped: true,
		},
		{
			name:  "context line after an edit moves by the insertions above it",
			patch: rewrite, oldLine: 3, want: 6, mapped: true,
		},
		{
			name:  "a patch with no hunk maps nothing",
			patch: "no hunks here", oldLine: 2, want: 0, mapped: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, mapped := diff.MapLineToNewSide(testCase.patch, testCase.oldLine)
			if mapped != testCase.mapped {
				t.Fatalf("mapped = %t, want %t", mapped, testCase.mapped)
			}
			if mapped && got != testCase.want {
				t.Fatalf("old line %d mapped to %d, want %d", testCase.oldLine, got, testCase.want)
			}
		})
	}
}

const (
	testHeadSHA      = "a3c4f1cac7f595bc824704b9d2a1f1191630dc32"
	testStaleHeadSHA = "b4d5e2dbd8f606cd935815c0e3b2f2202741ed43"
)

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

	changed, _, err := diff.ChangedRightLines(patch)
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

	changed, _, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatalf("ChangedRightLines: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("deleted-only hunk changed lines = %v, want empty", changed)
	}
}

func TestChangedRightLinesMalformedHeader(t *testing.T) {
	_, _, err := diff.ChangedRightLines("@@ not-a-hunk @@\n context")
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

	changed, _, err := diff.ChangedRightLines(patch)
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

	changed, hunks, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatalf("ChangedRightLines: %v", err)
	}
	if !diff.ValidRange(changed, hunks, 2, 2) {
		t.Fatal("single added line should be valid")
	}
	if !diff.ValidRange(changed, hunks, 2, 4) {
		t.Fatal("contiguous added lines in one hunk should be valid")
	}
	if diff.ValidRange(changed, hunks, 2, 5) {
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

	changed, hunks, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatalf("ChangedRightLines: %v", err)
	}
	if diff.ValidRange(changed, hunks, 2, 4) {
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

// The delta is the unit of work: a range since a previously reviewed commit
// must compare against that commit rather than list the whole pull request
// again, so a run never reviews the same commit range twice.
func TestCollectorRangeComparesSinceTheGivenBase(t *testing.T) {
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
	base := domain.HeadSHA(testStaleHeadSHA)
	pullRequest := testPullRequest()

	input, err := collector.CollectRange(context.Background(), testRef(), pullRequest, base)
	if err != nil {
		t.Fatalf("CollectRange: %v", err)
	}
	if source.compareCalls != 1 {
		t.Fatalf("compare calls = %d, want 1", source.compareCalls)
	}
	if source.listCalls != 0 {
		t.Fatalf("list changed files calls = %d, want 0: the range must not fetch the full file list", source.listCalls)
	}
	if source.lastCompareBase != base || source.lastCompareHead != pullRequest.Head {
		t.Fatalf("compare range = %s...%s, want %s...%s",
			source.lastCompareBase, source.lastCompareHead, base, pullRequest.Head)
	}
	if len(input.Files) != 1 {
		t.Fatalf("file count = %d, want 1", len(input.Files))
	}
}

// An empty base means no prior review exists, so the range collector falls
// back to the full file list rather than comparing against nothing.
func TestCollectorRangeFallsBackToTheFullListWhenBaseIsEmpty(t *testing.T) {
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

	input, err := collector.CollectRange(context.Background(), testRef(), testPullRequest(), "")
	if err != nil {
		t.Fatalf("CollectRange: %v", err)
	}
	if source.compareCalls != 0 {
		t.Fatalf("compare calls = %d, want 0: an empty base means no prior review", source.compareCalls)
	}
	if source.listCalls != 1 {
		t.Fatalf("list changed files calls = %d, want 1", source.listCalls)
	}
	if len(input.Files) != 1 {
		t.Fatalf("file count = %d, want 1", len(input.Files))
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

func TestEndToEndMoreThanOneHundredChangedFiles(t *testing.T) {
	files := make([]githubapp.ChangedFile, 0, 101)
	contents := make(map[string][]byte, 101)
	for index := range 101 {
		path := fmt.Sprintf("internal/pkg/file_%03d.go", index)
		files = append(files, githubapp.ChangedFile{
			Path:         path,
			Status:       "modified",
			Patch:        "@@ -1 +1,2 @@\n x\n+y\n",
			PatchPresent: true,
		})
		contents[path] = []byte("x\ny\n")
	}

	source := &fakeSource{files: files, contents: contents}
	input, err := diff.NewCollector(source).Collect(context.Background(), testRef(), testPullRequest())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(input.Files) != 101 {
		t.Fatalf("file count = %d, want 101", len(input.Files))
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
	files           []githubapp.ChangedFile
	contents        map[string][]byte
	getErr          error
	compareErr      error
	getCalls        int
	listCalls       int
	compareCalls    int
	lastCompareBase domain.HeadSHA
	lastCompareHead domain.HeadSHA
}

func (source *fakeSource) ListChangedFiles(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	_ int,
) ([]githubapp.ChangedFile, error) {
	source.listCalls++
	return source.files, nil
}

func (source *fakeSource) Compare(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	base domain.HeadSHA,
	head domain.HeadSHA,
) (githubapp.Comparison, error) {
	source.compareCalls++
	source.lastCompareBase = base
	source.lastCompareHead = head
	if source.compareErr != nil {
		return githubapp.Comparison{}, source.compareErr
	}
	return githubapp.Comparison{MergeBase: base, Files: source.files}, nil
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

func TestChunkSplitDividesHunksAndKeepsEveryLine(t *testing.T) {
	patch := strings.Join([]string{
		"@@ -1,2 +1,3 @@",
		" package main",
		"+added1",
		"@@ -20,2 +21,3 @@",
		" func b() {}",
		"+added2",
		"@@ -40,2 +41,3 @@",
		" func c() {}",
		"+added3",
	}, "\n")
	changed, hunks, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatalf("ChangedRightLines: %v", err)
	}

	chunks, err := diff.ChunkInput(diff.ReviewInput{
		PullRequest: testPullRequest(),
		Files: []diff.FileContext{{
			Path:              "main.go",
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    "package main\n",
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		}},
	}, 1_000_000)
	if err != nil {
		t.Fatalf("ChunkInput: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunk count = %d, want 1", len(chunks))
	}
	whole := chunks[0]
	if len(whole.Pieces) != 3 {
		t.Fatalf("pieces = %d, want one per hunk", len(whole.Pieces))
	}

	first, second, ok := whole.Split()
	if !ok {
		t.Fatal("Split() reported false for a chunk with three hunks")
	}
	if len(first.Pieces)+len(second.Pieces) != len(whole.Pieces) {
		t.Fatalf("halves hold %d and %d pieces, want %d together",
			len(first.Pieces), len(second.Pieces), len(whole.Pieces))
	}
	for _, piece := range whole.Pieces {
		if !strings.Contains(first.Text, piece.Text) && !strings.Contains(second.Text, piece.Text) {
			t.Fatalf("a hunk was lost by the split: %q", piece.Text)
		}
	}
	if first.Index != whole.Index || second.Total != whole.Total {
		t.Fatal("the halves lost the parent chunk position")
	}
}

func TestChunkSplitStopsAtOneHunk(t *testing.T) {
	single := diff.Chunk{
		Index:            1,
		Total:            1,
		Text:             "one hunk",
		Pieces:           []diff.Piece{{Path: "main.go", Text: "one hunk", CoverageComplete: true}},
		Paths:            []string{"main.go"},
		CoverageComplete: true,
	}

	if _, _, ok := single.Split(); ok {
		t.Fatal("Split() reported true for a chunk holding one hunk")
	}
}

func TestChunkSplitCarriesIncompleteCoverage(t *testing.T) {
	chunk := diff.Chunk{
		Index: 1,
		Total: 1,
		Text:  "a\nb",
		Pieces: []diff.Piece{
			{Path: "a.go", Text: "a", CoverageComplete: true},
			{Path: "b.go", Text: "b", CoverageComplete: false},
		},
		Paths:            []string{"a.go", "b.go"},
		CoverageComplete: false,
	}

	first, second, ok := chunk.Split()
	if !ok {
		t.Fatal("Split() reported false for a chunk with two hunks")
	}
	if !first.CoverageComplete {
		t.Fatal("the complete half reports incomplete coverage")
	}
	if second.CoverageComplete {
		t.Fatal("the incomplete half reports complete coverage")
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

// A force push can leave the recorded base unreachable, and GitHub then refuses
// the comparison outright. Failing there would strand the pull request: every
// later run reads the same dead base from the marker and aborts the same way,
// so the review never recovers on its own.
func TestCollectorRangeReviewsEverythingWhenTheBaseIsGone(t *testing.T) {
	source := &fakeSource{
		files: []githubapp.ChangedFile{{
			Path:         "pkg/a.go",
			Status:       "modified",
			Patch:        "@@ -1,1 +1,2 @@\n line\n+added\n",
			PatchPresent: true,
		}},
		contents:   map[string][]byte{"pkg/a.go": []byte("line\nadded\n")},
		compareErr: githubapp.APIError{StatusCode: http.StatusNotFound, Message: "No common ancestor"},
	}
	collector := diff.NewCollector(source)

	input, err := collector.CollectRange(
		context.Background(), testRef(), testPullRequest(), domain.HeadSHA(testStaleHeadSHA),
	)
	if err != nil {
		t.Fatalf("CollectRange: %v, want the whole pull request reviewed instead of a failure", err)
	}
	if source.listCalls != 1 {
		t.Fatalf("list changed files calls = %d, want 1: the run must fall back to the full list", source.listCalls)
	}
	if len(input.Files) != 1 {
		t.Fatalf("file count = %d, want 1", len(input.Files))
	}
}

// A transient failure must keep failing, so a later run retries the range
// rather than silently reviewing more than it was asked to.
func TestCollectorRangeFailsWhenCompareIsMerelyBroken(t *testing.T) {
	source := &fakeSource{
		compareErr: githubapp.APIError{StatusCode: http.StatusInternalServerError, Message: "server error"},
	}
	collector := diff.NewCollector(source)

	if _, err := collector.CollectRange(
		context.Background(), testRef(), testPullRequest(), domain.HeadSHA(testStaleHeadSHA),
	); err == nil {
		t.Fatal("CollectRange returned no error, want the transient failure surfaced")
	}
	if source.listCalls != 0 {
		t.Fatalf("list changed files calls = %d, want 0", source.listCalls)
	}
}

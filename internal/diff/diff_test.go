package diff_test

import (
	"fmt"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/githubapp"
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

// A small hunk uses nearby context when its full file exceeds the prompt.
func TestChunkInputBoundsCurrentContentAroundEachHunk(t *testing.T) {
	const maximumBytes = 400
	lines := make([]string, 200)
	for index := range lines {
		lines[index] = fmt.Sprintf("distant-line-%03d", index+1)
	}
	lines[99] = "unchanged"
	lines[100] = "added"

	input := diff.ReviewInput{
		PullRequest: githubapp.PullRequest{},
		Files: []diff.FileContext{{
			Path:              "a.go",
			Status:            "modified",
			Patch:             "@@ -100,1 +100,2 @@\n unchanged\n+added\n",
			CurrentContent:    strings.Join(lines, "\n") + "\n",
			ChangedRightLines: nil,
			ChangedRightHunks: nil,
			CoverageComplete:  true,
			Gap:               diff.CoverageGapNone,
		}},
		MergeBase: "",
	}

	chunks, err := diff.ChunkInput(input, maximumBytes)
	if err != nil {
		t.Fatalf("ChunkInput: %v", err)
	}
	if len(chunks) != 1 || len(chunks[0].Pieces) != 1 {
		t.Fatalf("chunks = %d, pieces = %d, want one chunk with one hunk",
			len(chunks), len(chunks[0].Pieces))
	}
	piece := chunks[0].Pieces[0]
	if piece.Oversized || !piece.CoverageComplete {
		t.Fatalf("small hunk = oversized %t, complete %t, want reviewable",
			piece.Oversized, piece.CoverageComplete)
	}
	if len(piece.Text) > maximumBytes {
		t.Fatalf("hunk bytes = %d, want at most %d", len(piece.Text), maximumBytes)
	}
	for _, expected := range []string{"distant-line-099", "unchanged", "added", "distant-line-102"} {
		if !strings.Contains(piece.Text, expected) {
			t.Fatalf("hunk text omits nearby line %q: %q", expected, piece.Text)
		}
	}
	if strings.Contains(piece.Text, "distant-line-001") {
		t.Fatalf("hunk text includes the whole file: %q", piece.Text)
	}
}

// A hunk outside the current source is refused instead of reviewed without context.
func TestChunkInputRefusesMismatchedHunkCoordinates(t *testing.T) {
	const maximumBytes = 400
	input := diff.ReviewInput{
		PullRequest: githubapp.PullRequest{},
		Files: []diff.FileContext{{
			Path:              "a.go",
			Status:            "modified",
			Patch:             "@@ -500,1 +500,2 @@\n absent\n+added\n",
			CurrentContent:    strings.Repeat("current line\n", 200),
			ChangedRightLines: nil,
			ChangedRightHunks: nil,
			CoverageComplete:  true,
			Gap:               diff.CoverageGapNone,
		}},
		MergeBase: "",
	}

	chunks, err := diff.ChunkInput(input, maximumBytes)
	if err != nil {
		t.Fatalf("ChunkInput: %v", err)
	}
	if len(chunks) != 1 || len(chunks[0].Pieces) != 1 {
		t.Fatalf("chunks = %d, pieces = %d, want one chunk with one hunk",
			len(chunks), len(chunks[0].Pieces))
	}
	piece := chunks[0].Pieces[0]
	if !piece.Oversized || piece.CoverageComplete {
		t.Fatalf("mismatched hunk = oversized %t, complete %t, want refused",
			piece.Oversized, piece.CoverageComplete)
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

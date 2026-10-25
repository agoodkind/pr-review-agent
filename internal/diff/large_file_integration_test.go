package diff_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
)

const largeGitPromptBytes = 12000

type largeGitChange struct {
	before  string
	after   string
	status  string
	added   map[int]string
	removed []string
}

func TestLargeGitHunkRetainsChangedRowsWithinPromptBudget(t *testing.T) {
	for _, kind := range []string{"addition", "replacement", "deletion", "removed", "missing newline"} {
		t.Run(kind, func(t *testing.T) {
			change := largeGitFixture(kind, 450)
			input := largeGitReviewInput(t, change)
			if kind == "missing newline" && strings.Count(input.Files[0].Patch, `\ No newline at end of file`) != 2 {
				t.Fatal("The Git fixture must change both unterminated final lines.")
			}
			chunks, err := diff.ChunkInput(input, largeGitPromptBytes)
			if err != nil {
				t.Fatal(err)
			}
			policy := policytest.Load(t)
			bounded, err := review.BoundReviewChunks(chunks, input.Files, 7, "", policy, largeGitPromptBytes)
			if err != nil {
				t.Fatal(err)
			}
			if len(bounded) < 2 {
				t.Fatal("The large Git change must require several complete requests.")
			}
			for _, chunk := range bounded {
				prompt, err := review.BuildPrompt(chunk, 7, "", policy, largeGitPromptBytes)
				if err != nil {
					t.Fatal(err)
				}
				if len(prompt) > largeGitPromptBytes {
					t.Fatalf("The rendered request requires %d bytes; the budget is %d.", len(prompt), largeGitPromptBytes)
				}
			}
			assertLargeGitCoverage(t, change, input, bounded)
		})
	}
}

func TestSingleGitHunkSplitsRecursivelyWithOriginalCoordinates(t *testing.T) {
	change := largeGitFixture("replacement", 50)
	input := largeGitReviewInput(t, change)
	chunks, err := diff.ChunkInput(input, len(input.Files[0].Patch)*4)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 || len(chunks[0].Pieces) != 1 {
		t.Fatal("The Git fixture must begin as one complete hunk.")
	}
	parts := splitLargeGitChunk(t, chunks[0], 0)
	if len(parts) < 2 {
		t.Fatal("The original hunk did not split into independently reviewable parts.")
	}
	assertLargeGitCoverage(t, change, input, parts)
}

func TestGitPrefixDeletionKeepsBoundedContextAtZeroNewStart(t *testing.T) {
	deleted := make([]string, 100)
	for index := range deleted {
		deleted[index] = fmt.Sprintf("deleted prefix line %03d", index)
	}
	surviving := make([]string, 1000)
	for index := range surviving {
		surviving[index] = fmt.Sprintf("surviving source line %04d %s", index, strings.Repeat("s", 60))
	}
	after := strings.Join(surviving, "\n") + "\n"
	change := largeGitChange{before: strings.Join(deleted, "\n") + "\n" + after, after: after, status: "modified", added: map[int]string{}, removed: deleted}
	input := largeGitReviewInput(t, change)
	if !strings.Contains(input.Files[0].Patch, "@@ -1,100 +0,0 @@") {
		t.Fatal("The Git fixture must delete a prefix with a zero new-side start.")
	}
	chunks, err := diff.ChunkInput(input, largeGitPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	policy := policytest.Load(t)
	bounded, err := review.BoundReviewChunks(chunks, input.Files, 7, "", policy, largeGitPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range bounded {
		if _, err := review.BuildPrompt(chunk, 7, "", policy, largeGitPromptBytes); err != nil {
			t.Fatal(err)
		}
	}
	assertLargeGitCoverage(t, change, input, bounded)
	if !strings.Contains(bounded[0].Text, surviving[0]) {
		t.Fatal("The deletion review must include context from the surviving file's first line.")
	}
}

func TestIndivisibleGitLineRetainsAnExplicitCoverageGap(t *testing.T) {
	line := strings.Repeat("x", largeGitPromptBytes*3)
	change := largeGitChange{before: "", after: line + "\n", status: "added", added: map[int]string{1: line}, removed: nil}
	input := largeGitReviewInput(t, change)
	chunks, err := diff.ChunkInput(input, largeGitPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	policy := policytest.Load(t)
	bounded, err := review.BoundReviewChunks(chunks, input.Files, 7, "", policy, largeGitPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != 1 || bounded[0].CoverageComplete || !bounded[0].Pieces[0].Oversized {
		t.Fatal("The indivisible line must remain an explicit coverage gap.")
	}
	rows, err := diff.ChangedSourceRows(bounded[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatal("The unread line must not appear as reviewed source.")
	}
	if _, err := review.BuildPrompt(bounded[0], 7, "", policy, largeGitPromptBytes); err != nil {
		t.Fatalf("The gap report must fit the complete request budget: %v", err)
	}
}

func splitLargeGitChunk(t *testing.T, chunk diff.Chunk, depth int) []diff.Chunk {
	t.Helper()
	if depth > 32 {
		t.Fatal("Recursive hunk division did not terminate.")
	}
	first, second, ok := chunk.Split()
	if !ok {
		return []diff.Chunk{chunk}
	}
	return append(splitLargeGitChunk(t, first, depth+1), splitLargeGitChunk(t, second, depth+1)...)
}

func assertLargeGitCoverage(t *testing.T, change largeGitChange, input diff.ReviewInput, chunks []diff.Chunk) {
	t.Helper()
	added := make(map[int]string)
	var removed []string
	newlineMarkers := 0
	var fragments []string
	for _, chunk := range chunks {
		if !chunk.CoverageComplete {
			t.Fatal("A divisible Git change lost complete coverage.")
		}
		rows, err := diff.ChangedSourceRows(chunk)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Path != input.Files[0].Path || row.Surface != domain.FindingFile {
				t.Fatalf("The changed row has an incorrect file identity: %+v", row)
			}
			if _, exists := added[row.RightLine]; exists {
				t.Fatalf("The added line %d appears in several fragments.", row.RightLine)
			}
			added[row.RightLine] = row.Text
		}
		for _, piece := range chunk.Pieces {
			fragments = append(fragments, piece.Patch)
			if piece.Oversized || !piece.CoverageComplete {
				t.Fatal("A divisible Git fragment became an omission.")
			}
			if change.after != "" {
				if err := diff.ValidatePatchContent(piece.Patch, change.after); err != nil {
					t.Fatalf("The fragment does not match the actual head file: %v", err)
				}
			}
			for _, line := range strings.Split(piece.Patch, "\n")[1:] {
				if strings.HasPrefix(line, "-") {
					removed = append(removed, line[1:])
				}
				if line == `\ No newline at end of file` {
					newlineMarkers++
				}
			}
		}
	}
	if !reflect.DeepEqual(added, change.added) {
		t.Fatalf("The fragments retained %d added rows; expected %d with exact original coordinates and content.", len(added), len(change.added))
	}
	if !reflect.DeepEqual(removed, change.removed) {
		t.Fatalf("The fragments retained %d deleted rows; expected %d in original order.", len(removed), len(change.removed))
	}
	if newlineMarkers != strings.Count(input.Files[0].Patch, `\ No newline at end of file`) {
		t.Fatal("Fragment division lost a Git final-newline marker.")
	}
	assertLargeGitApply(t, change, fragments)
}

func assertLargeGitApply(t *testing.T, change largeGitChange, fragments []string) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "changed.go")
	if change.before != "" {
		if err := os.WriteFile(path, []byte(change.before), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldPath, newPath := "a/changed.go", "b/changed.go"
	if change.status == "added" {
		oldPath = "/dev/null"
		// Git prepends fragments that share the empty base's insertion position.
		slices.Reverse(fragments)
	}
	if change.status == "removed" {
		newPath = "/dev/null"
	}
	patch := fmt.Sprintf("--- %s\n+++ %s\n%s\n", oldPath, newPath, strings.Join(fragments, "\n"))
	command := exec.CommandContext(t.Context(), "git", "apply", "--unidiff-zero")
	command.Dir = directory
	command.Stdin = strings.NewReader(patch)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Git could not apply the review fragments at their original coordinates: %v\n%s", err, output)
	}
	if change.status == "removed" {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("Applying the removal fragments did not delete the original file.")
		}
		return
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != change.after {
		t.Fatalf("Applying the review fragments produced %d bytes starting %.80q; expected %d bytes starting %.80q.", len(content), content, len(change.after), change.after)
	}
}

func largeGitFixture(kind string, count int) largeGitChange {
	prefix := []string{"package example", "", "// Shared source context.", "func update() {"}
	suffix := []string{"}", "// Unchanged tail."}
	oldLines := make([]string, 0, count)
	newLines := make([]string, 0, count)
	for index := range count {
		oldLines = append(oldLines, fmt.Sprintf("old_%04d := \"previous value %s\"", index, strings.Repeat("x", 40)))
		newLines = append(newLines, fmt.Sprintf("new_%04d := \"café 雪 %s\"", index, strings.Repeat("y", 40)))
	}
	change := largeGitChange{before: "", after: "", status: "modified", added: make(map[int]string), removed: nil}
	before := append(append(append([]string{}, prefix...), oldLines...), suffix...)
	after := append(append(append([]string{}, prefix...), newLines...), suffix...)
	switch kind {
	case "addition":
		before = nil
		after = newLines
		change.status = "added"
	case "deletion":
		after = append(append([]string{}, prefix...), suffix...)
	case "removed":
		before = oldLines
		after = nil
		change.status = "removed"
	case "missing newline":
		before = append(append([]string{}, prefix...), oldLines...)
		after = append(append([]string{}, prefix...), newLines...)
	}
	if before != nil {
		change.before = strings.Join(before, "\n") + "\n"
	}
	if after != nil {
		change.after = strings.Join(after, "\n") + "\n"
	}
	if kind == "missing newline" {
		change.before = strings.TrimSuffix(change.before, "\n")
		change.after = strings.TrimSuffix(change.after, "\n")
	}
	if kind == "addition" || kind == "replacement" || kind == "missing newline" {
		start := len(prefix) + 1
		if kind == "addition" {
			start = 1
		}
		for index, line := range newLines {
			change.added[start+index] = line
		}
	}
	if kind != "addition" {
		change.removed = oldLines
	}
	return change
}

func largeGitReviewInput(t *testing.T, change largeGitChange) diff.ReviewInput {
	t.Helper()
	directory := t.TempDir()
	largeGitCommand(t, directory, "init", "--quiet")
	path := filepath.Join(directory, "changed.go")
	if change.before != "" {
		if err := os.WriteFile(path, []byte(change.before), 0o600); err != nil {
			t.Fatal(err)
		}
		largeGitCommand(t, directory, "add", "--", "changed.go")
	}
	largeGitCommit(t, directory, "Create base content")
	base := strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", "HEAD"))
	if change.after == "" {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(path, []byte(change.after), 0o600); err != nil {
		t.Fatal(err)
	}
	largeGitCommand(t, directory, "add", "--all")
	largeGitCommit(t, directory, "Update changed content")
	patch := largeGitCommand(t, directory, "diff", "--no-ext-diff", "--no-renames", "--unified=0", base, "HEAD", "--", "changed.go")
	changed, hunks, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatal(err)
	}
	return diff.ReviewInput{PullRequest: githubapp.PullRequest{}, Metadata: nil, MergeBase: domain.HeadSHA(base), Files: []diff.FileContext{{
		Path: "changed.go", Status: change.status, Patch: patch, CurrentContent: change.after,
		ChangedRightLines: changed, ChangedRightHunks: hunks, CoverageComplete: true, Gap: diff.CoverageGapNone,
	}}}
}

func largeGitCommit(t *testing.T, directory, subject string) {
	t.Helper()
	largeGitCommand(t, directory, "-c", "user.name=Integration Test", "-c", "user.email=integration@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "--allow-empty", "-m", subject+"\n\nCo-authored-by: Codex <noreply@openai.com>")
}

func largeGitCommand(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Git %s failed: %v\n%s", arguments[0], err, output)
	}
	return string(output)
}

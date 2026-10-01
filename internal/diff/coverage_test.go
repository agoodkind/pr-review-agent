package diff_test

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func TestChunkInputMarksAnOversizedHunkAndNamesIt(t *testing.T) {
	const small = "@@ -1,1 +1,2 @@\n a\n+one\n"
	huge := "@@ -4,1 +5,3 @@\n d\n+" + strings.Repeat("x", 400) + "\n+" + strings.Repeat("y", 400) + "\n"
	input := diff.ReviewInput{
		PullRequest: githubapp.PullRequest{},
		Files: []diff.FileContext{{
			Path:              "a.go",
			Status:            "modified",
			Patch:             small + "\n" + huge,
			CurrentContent:    "a\none\nb\nc\nd\n",
			ChangedRightLines: nil,
			ChangedRightHunks: nil,
			CoverageComplete:  true,
			Gap:               diff.CoverageGapNone,
		}},
		MergeBase: "",
	}

	chunks, err := diff.ChunkInput(input, 500)
	if err != nil {
		t.Fatalf("ChunkInput: %v", err)
	}
	pieces := make([]diff.Piece, 0, 2)
	for _, chunk := range chunks {
		pieces = append(pieces, chunk.Pieces...)
	}
	if len(pieces) != 2 {
		t.Fatalf("pieces = %d, want one per hunk", len(pieces))
	}
	if pieces[0].Oversized {
		t.Fatal("the small hunk was marked oversized")
	}
	if pieces[0].Header != "@@ -1,1 +1,2 @@" {
		t.Fatalf("small hunk header = %q, want its coordinates", pieces[0].Header)
	}
	if !pieces[1].Oversized {
		t.Fatal("the hunk larger than a whole chunk was not marked oversized")
	}
	if pieces[1].CoverageComplete {
		t.Fatal("an oversized hunk reported complete coverage")
	}
	if pieces[1].Header != "@@ -4,1 +5,3 @@" {
		t.Fatalf("oversized hunk header = %q, want its coordinates", pieces[1].Header)
	}
	if strings.Contains(pieces[1].Text, "xxxx") {
		t.Fatalf("oversized hunk text = %q, want a placeholder rather than the diff", pieces[1].Text)
	}
}

// The coordinates a hunk is named by carry no source. Git appends the enclosing
// line to a hunk header, and that line is published on a pull request when a
// hunk goes unread, so it is dropped rather than reprinted.
func TestAnUnreadHunkIsNamedWithoutTheSourceGitAppends(t *testing.T) {
	patch := "@@ -1,1 +1,2 @@ func secret(token string) {\n a\n+" + strings.Repeat("z", 400) + "\n"
	input := diff.ReviewInput{
		PullRequest: githubapp.PullRequest{},
		Files: []diff.FileContext{{
			Path:              "a.go",
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    "a\n",
			ChangedRightLines: nil,
			ChangedRightHunks: nil,
			CoverageComplete:  true,
			Gap:               diff.CoverageGapNone,
		}},
		MergeBase: "",
	}

	chunks, err := diff.ChunkInput(input, 300)
	if err != nil {
		t.Fatalf("ChunkInput: %v", err)
	}
	if len(chunks) != 1 || len(chunks[0].Pieces) != 1 {
		t.Fatalf("chunks = %d, want one holding the one hunk", len(chunks))
	}
	piece := chunks[0].Pieces[0]
	if piece.Header != "@@ -1,1 +1,2 @@" {
		t.Fatalf("header = %q, want the coordinates alone", piece.Header)
	}
	if strings.Contains(piece.Header, "func secret") {
		t.Fatalf("header = %q, want no source line from the file", piece.Header)
	}
}

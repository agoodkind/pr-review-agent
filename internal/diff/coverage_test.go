package diff_test

// These tests are about the difference between a gap a later run would close
// and one it would hit again, because the review service ends a run one way or
// the other on exactly that answer.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// A file that could not be collected whole says why, and says whether a later
// run would fare any better. A binary file, a file GitHub supplies no patch
// for, and a patch that cannot be read whole come back the same way every time;
// content that failed to load is one call that went wrong.
func TestCollectorNamesWhyAFileWasNotReadWhole(t *testing.T) {
	const goodPatch = "@@ -1,1 +1,2 @@\n line\n+added\n"
	cases := []struct {
		name       string
		file       githubapp.ChangedFile
		getErr     error
		contents   map[string][]byte
		wantGap    diff.CoverageGap
		wantRecurs bool
	}{
		{
			name: "a binary file carries no reviewable patch",
			file: githubapp.ChangedFile{
				Path: "image.png", Status: "binary", Patch: "", PatchPresent: false,
			},
			getErr: nil, contents: nil,
			wantGap: diff.CoverageGapBinary, wantRecurs: true,
		},
		{
			name: "github supplies no patch for an oversized file",
			file: githubapp.ChangedFile{
				Path: "big.bin", Status: "modified", Patch: "", PatchPresent: false,
			},
			getErr: nil, contents: nil,
			wantGap: diff.CoverageGapPatchAbsent, wantRecurs: true,
		},
		{
			name: "a malformed hunk header cannot be read",
			file: githubapp.ChangedFile{
				Path: "pkg/a.go", Status: "modified", Patch: "@@ nonsense @@\n+x\n", PatchPresent: true,
			},
			getErr: nil, contents: nil,
			wantGap: diff.CoverageGapPatchUnreadable, wantRecurs: true,
		},
		{
			// A patch whose counts do not add up is a truncated patch, and a file
			// GitHub truncates the patch for is one whose content load tends to
			// fail too. The permanent reason has to survive the temporary one, or
			// the run reports a file it can never read as one it will retry.
			name: "a truncated patch keeps its reason through a failed content load",
			file: githubapp.ChangedFile{
				Path: "pkg/a.go", Status: "modified", Patch: "@@ -1,1 +1,5 @@\n line\n+added\n", PatchPresent: true,
			},
			getErr: errors.New("read failed"), contents: nil,
			wantGap: diff.CoverageGapPatchUnreadable, wantRecurs: true,
		},
		{
			name: "a content read that got no answer is one call that went wrong",
			file: githubapp.ChangedFile{
				Path: "pkg/a.go", Status: "modified", Patch: goodPatch, PatchPresent: true,
			},
			getErr:   githubapp.APIError{StatusCode: http.StatusBadGateway, Message: "bad gateway"},
			contents: nil,
			wantGap:  diff.CoverageGapContentUnavailable, wantRecurs: false,
		},
		{
			// GitHub answered, and it answers the same way on every later run.
			// Calling this temporary leaves the head incomplete forever while
			// naming nothing a person could act on.
			name: "a file github will not serve is answered the same way every time",
			file: githubapp.ChangedFile{
				Path: "pkg/a.go", Status: "modified", Patch: goodPatch, PatchPresent: true,
			},
			getErr:   githubapp.APIError{StatusCode: http.StatusNotFound, Message: "Not Found"},
			contents: nil,
			wantGap:  diff.CoverageGapContentMissing, wantRecurs: true,
		},
		{
			// An error carrying no status this service recognizes could be
			// either. Calling it permanent would tell a person to split a pull
			// request over something that may pass on the next run.
			name: "an unclassifiable content failure is treated as one that may pass",
			file: githubapp.ChangedFile{
				Path: "pkg/a.go", Status: "modified", Patch: goodPatch, PatchPresent: true,
			},
			getErr: errors.New("read failed"), contents: nil,
			wantGap: diff.CoverageGapContentUnavailable, wantRecurs: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := &fakeSource{
				files:    []githubapp.ChangedFile{testCase.file},
				contents: testCase.contents,
				getErr:   testCase.getErr,
			}
			input, err := diff.NewCollector(source).Collect(
				context.Background(), testRef(), testPullRequest(),
			)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			file := input.Files[0]
			if file.CoverageComplete {
				t.Fatal("coverage complete = true, want a file reported as not read whole")
			}
			if file.Gap != testCase.wantGap {
				t.Fatalf("gap = %q, want %q", file.Gap, testCase.wantGap)
			}
			if file.Gap.Recurs() != testCase.wantRecurs {
				t.Fatalf("recurs = %t, want %t", file.Gap.Recurs(), testCase.wantRecurs)
			}
		})
	}
}

// A file collected whole names no gap at all, so nothing downstream mistakes a
// complete read for a shortfall it has to report.
func TestACompletelyReadFileNamesNoGap(t *testing.T) {
	source := &fakeSource{
		files: []githubapp.ChangedFile{{
			Path:         "pkg/a.go",
			Status:       "modified",
			Patch:        "@@ -1,1 +1,2 @@\n line\n+added\n",
			PatchPresent: true,
		}},
		contents: map[string][]byte{"pkg/a.go": []byte("line\nadded\n")},
	}
	input, err := diff.NewCollector(source).Collect(context.Background(), testRef(), testPullRequest())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if input.Files[0].Gap != diff.CoverageGapNone {
		t.Fatalf("gap = %q, want none", input.Files[0].Gap)
	}
	if input.Files[0].Gap.Recurs() {
		t.Fatal("recurs = true, want false for a file that was read whole")
	}
}

// A hunk larger than a whole chunk is replaced by a placeholder, so the piece
// that stands for it has to say so and name the hunk. Nothing splits a hunk, so
// this is the one shortfall a later run cannot close.
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

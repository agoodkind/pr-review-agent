package review

// These cover what the unread hunk notice publishes, which carries two things
// this service did not write: a repository path and a hunk header.

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"goodkind.io/pr-review-agent/internal/domain"
)

const noticeTestHead = "a3c4f1cac7f595bc824704b9d2a1f1191630dc32"

// A repository path cannot close the code fence or inject Markdown.
func TestAnUnreadHunkCannotInjectMarkdownThroughItsPath(t *testing.T) {
	hostile := "```\nBODY: [click](https://evil.example)/main.go"

	notice := structuralShortfallNotice(
		domain.HeadSHA(noticeTestHead),
		structuralShortfall{Hunks: []unreadHunk{{
			Path:   hostile,
			Header: "@@ -1,1 +1,2 @@ `evil`",
			Reason: oversizedHunkReason,
		}}},
		0,
	)

	if strings.Contains(notice, "\nBODY:") {
		t.Fatalf("a newline in a path reached the rendered notice:\n%s", notice)
	}
	if !strings.Contains(notice, `\n`) {
		t.Fatalf("the newline was dropped rather than escaped:\n%s", notice)
	}
	// The renderer must emit exactly one opening and one closing fence.
	if opened := strings.Count(notice, "\n```"); opened != 2 {
		t.Fatalf("fence lines = %d, want one opened and one closed:\n%s", opened, notice)
	}
}

// A long Unicode path remains valid UTF-8 after truncation.
func TestUnreadHunkTruncationKeepsAWholeRune(t *testing.T) {
	label := describeUnreadHunk(unreadHunk{
		Path:   strings.Repeat("界", maximumUnreadHunkLabelBytes),
		Header: "",
		Reason: "",
	})

	if !utf8.ValidString(label) {
		t.Fatalf("label contains a partial UTF-8 rune: %q", label)
	}
	if len(label) > maximumUnreadHunkLabelBytes+len("...") {
		t.Fatalf("label bytes = %d, want at most %d", len(label), maximumUnreadHunkLabelBytes+len("..."))
	}
}

// A pull request touching hundreds of files must still produce a check a reader
// can open. GitHub caps the output by size, so the list is bounded while the
// count in the sentence above it stays exact.
func TestALongUnreadListIsBoundedAndSaysHowManyItOmitted(t *testing.T) {
	const extra = 5
	hunks := make([]unreadHunk, 0, maximumListedUnreadHunks+extra)
	for index := 0; index < maximumListedUnreadHunks+extra; index++ {
		hunks = append(hunks, unreadHunk{
			Path:   fmt.Sprintf("pkg/%s%d.go", strings.Repeat("deep/", 40), index),
			Header: "@@ -1,1 +1,2 @@",
			Reason: oversizedHunkReason,
		})
	}

	notice := structuralShortfallNotice(
		domain.HeadSHA(noticeTestHead),
		structuralShortfall{Hunks: hunks},
		0,
	)

	if !strings.Contains(notice, hunkCount(maximumListedUnreadHunks+extra)+" this service cannot read") {
		t.Fatalf("the sentence lost the exact count:\n%s", notice)
	}
	if !strings.Contains(notice, fmt.Sprintf("and %d more not listed here.", extra)) {
		t.Fatalf("the notice does not say what it omitted:\n%s", notice)
	}
	if listed := strings.Count(notice, oversizedHunkReason); listed != maximumListedUnreadHunks {
		t.Fatalf("listed hunks = %d, want the cap of %d", listed, maximumListedUnreadHunks)
	}
	// One path long enough to crowd out the sentence is cut on its own.
	for _, line := range strings.Split(notice, "\n") {
		if len(line) > maximumUnreadHunkLabelBytes+len(oversizedHunkReason)+16 {
			t.Fatalf("one line ran past the label bound: %q", line)
		}
	}
}

// A run that also left chunks pending must not tell the reader the rest of the
// head was reviewed, because those chunks were not read either.
func TestANoticeWithPendingChunksDoesNotClaimTheRestWasReviewed(t *testing.T) {
	shortfall := structuralShortfall{Hunks: []unreadHunk{{
		Path:   "a.go",
		Header: "@@ -1,1 +1,2 @@",
		Reason: oversizedHunkReason,
	}}}

	withPending := structuralShortfallNotice(domain.HeadSHA(noticeTestHead), shortfall, 3)
	if strings.Contains(withPending, "Everything else on this head was reviewed") {
		t.Fatalf("the notice claims coverage it does not have:\n%s", withPending)
	}
	if !strings.Contains(withPending, "3 chunks went unread as well") {
		t.Fatalf("the notice does not say what is still owed:\n%s", withPending)
	}

	clean := structuralShortfallNotice(domain.HeadSHA(noticeTestHead), shortfall, 0)
	if !strings.Contains(clean, "Everything else on this head was reviewed") {
		t.Fatalf("a run owing nothing else should say so:\n%s", clean)
	}
}

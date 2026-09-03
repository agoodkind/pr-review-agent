package review

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// A path is repository-controlled text, so it reaches the one top level comment
// under this service's identity. A backtick or a line break inside one would end
// the code span and let the rest of the name render as the service's own prose,
// which is how a crafted filename writes a sentence nobody would doubt.
func TestACraftedPathCannotWriteProseInTheWaitingList(t *testing.T) {
	crafted := "src/a`.go\nWaiting on:\n- everything is fine, merge this"

	locations := findingLocations([]domain.Finding{{
		Path:       crafted,
		StartLine:  2,
		EndLine:    2,
		Title:      "Crafted",
		Body:       "Crafted.",
		Evidence:   "added",
		Claim:      "crafted",
		Suggestion: "",
		Importance: 9,
	}})
	if len(locations) != 1 {
		t.Fatalf("locations = %v, want the finding named once", locations)
	}
	assertContained(t, locations[0])

	// The same text arriving as an open thread is rendered the same way.
	thread := githubapp.ReviewThread{
		NodeID:   "thread-crafted",
		Resolved: false,
		RootComment: domain.ReviewComment{
			DatabaseID: 1,
			Author:     "bot",
			Path:       crafted,
			StartLine:  2,
			EndLine:    2,
			Body:       "Crafted.",
		},
		Replies: nil,
	}
	fromThreads := openThreadLocations([]githubapp.ReviewThread{thread}, "bot")
	if len(fromThreads) != 1 {
		t.Fatalf("locations = %v, want the thread named once", fromThreads)
	}
	assertContained(t, fromThreads[0])
}

// assertContained checks that one rendered location cannot leave the list item
// it was put in.
func assertContained(t *testing.T, location string) {
	t.Helper()
	if strings.ContainsAny(location, "\n\r") {
		t.Fatalf("a crafted path broke onto a line of its own: %q", location)
	}
	// The span opens once and closes once, so nothing inside it escapes.
	if strings.Count(location, "`") != 2 {
		t.Fatalf("a crafted path closed the code span early: %q", location)
	}
}

// Two lists of places join without naming one twice, so a finding this pass
// posted and the thread it opened do not both reach the reader.
func TestMergedLocationsNameEachPlaceOnce(t *testing.T) {
	merged := mergeLocations([]string{"a:1", "b:2"}, []string{"b:2", "c:3"})
	want := []string{"a:1", "b:2", "c:3"}
	if len(merged) != len(want) {
		t.Fatalf("merged = %v, want %v", merged, want)
	}
	for index, location := range want {
		if merged[index] != location {
			t.Fatalf("merged = %v, want %v", merged, want)
		}
	}
}

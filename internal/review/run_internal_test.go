package review

import (
	"context"
	"regexp"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/marker"
)

// chunkIDPattern is the shape the durable marker's pending list accepts. A
// chunk id that falls outside it encodes a marker the decoder throws away,
// which loses the pending list the next run resumes from.
var chunkIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

func testChunk(text string) diff.Chunk {
	return diff.Chunk{
		Index:            1,
		Total:            1,
		Text:             text,
		Pieces:           nil,
		Paths:            []string{"main.go"},
		CoverageComplete: true,
	}
}

// A chunk id has to survive being written into a marker and read back by a
// later run, so it must match what the marker's own decoder accepts.
func TestChunkIDIsTwelveLowercaseHexCharacters(t *testing.T) {
	id := chunkID(testChunk("File: main.go\nStatus: modified\n\n@@ -1,1 +1,2 @@\n+added\n"))

	if !chunkIDPattern.MatchString(id) {
		t.Fatalf("chunk id = %q, want twelve lowercase hex characters", id)
	}
}

// The id has to name the same chunk across runs, because a checkpoint written
// by one run is matched against a delta a later run re-derives. Position does
// not survive that, so the id comes from the text.
func TestChunkIDFollowsTheTextNotThePosition(t *testing.T) {
	first := testChunk("the same chunk text")
	moved := first
	moved.Index = 7
	moved.Total = 9

	if chunkID(first) != chunkID(moved) {
		t.Fatalf("chunk id changed with position: %q then %q", chunkID(first), chunkID(moved))
	}
	if chunkID(first) == chunkID(testChunk("different chunk text")) {
		t.Fatal("two different chunk texts share one id")
	}
}

func TestRemoveChunkIDDropsOnlyTheNamedID(t *testing.T) {
	remaining := removeChunkID([]string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "cccccccccccc"}, "bbbbbbbbbbbb")

	if len(remaining) != 2 || remaining[0] != "aaaaaaaaaaaa" || remaining[1] != "cccccccccccc" {
		t.Fatalf("remaining = %v, want the other two ids in order", remaining)
	}
}

// The work list is the delta minus what is already read. Both halves matter:
// subtracting nothing pays for every chunk again after one failure, and
// starting from the pending list instead of the delta skips the chunks a new
// commit introduced and then calls the range reviewed.
func TestPendingWorkIsTheDeltaMinusWhatIsAlreadyRead(t *testing.T) {
	first := testChunk("chunk one")
	second := testChunk("chunk two")
	fresh := testChunk("a chunk a later commit added")
	state := marker.State{
		LastReviewed: "",
		RunID:        "delivery-0",
		Status:       marker.StateReviewing,
		Pending:      []string{chunkID(second)},
		Completed:    []string{chunkID(first)},
	}

	work := pendingWork(context.Background(), state, []diff.Chunk{first, second, fresh})

	if len(work.owed) != 2 || work.owed[0] != chunkID(second) || work.owed[1] != chunkID(fresh) {
		t.Fatalf("owed = %v, want the pending chunk and the new one", work.owed)
	}
	if len(work.chunks) != 2 {
		t.Fatalf("chunks = %d, want only the ones still owed", len(work.chunks))
	}
	if len(work.completed) != 1 || work.completed[0] != chunkID(first) {
		t.Fatalf("completed = %v, want the already read chunk carried forward", work.completed)
	}
}

// The completed set keeps only ids the delta still holds. A chunk whose text
// has changed can never match again, so carrying its id would grow the marker
// for nothing; pruning bounds the set by the delta, which admission bounds.
func TestPendingWorkDropsCompletedIDsTheDeltaNoLongerHolds(t *testing.T) {
	surviving := testChunk("a chunk nobody touched")
	state := marker.State{
		LastReviewed: "",
		RunID:        "delivery-0",
		Status:       marker.StateReviewing,
		Pending:      nil,
		Completed:    []string{chunkID(surviving), chunkID(testChunk("a chunk since rewritten"))},
	}

	work := pendingWork(context.Background(), state, []diff.Chunk{surviving})

	if len(work.owed) != 0 {
		t.Fatalf("owed = %v, want none: the only chunk left was already read", work.owed)
	}
	if len(work.completed) != 1 || work.completed[0] != chunkID(surviving) {
		t.Fatalf("completed = %v, want only the id the delta still holds", work.completed)
	}
}

package marker

import (
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
)

func TestStateMarkerRoundTrip(t *testing.T) {
	original := State{
		LastReviewed: domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32"),
		RunID:        "delivery-abc",
		Status:       StateReviewing,
		Pending:      []string{"0011aabbccdd", "2233eeff0011"},
	}

	body := "## Review\n\nsummary prose\n\n" + EncodeState(original) + "\n"

	decoded, ok := DecodeState(body)
	if !ok {
		t.Fatal("DecodeState: marker not found")
	}
	if decoded.LastReviewed != original.LastReviewed {
		t.Fatalf("last reviewed = %q, want %q", decoded.LastReviewed, original.LastReviewed)
	}
	if decoded.RunID != original.RunID || decoded.Status != original.Status {
		t.Fatalf("run/status = %q/%q, want %q/%q", decoded.RunID, decoded.Status, original.RunID, original.Status)
	}
	if len(decoded.Pending) != 2 || decoded.Pending[0] != "0011aabbccdd" {
		t.Fatalf("pending = %v, want the two chunk ids", decoded.Pending)
	}
}

func TestStateMarkerWithNoPendingDecodesEmpty(t *testing.T) {
	body := EncodeState(State{
		LastReviewed: domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32"),
		RunID:        "delivery-abc",
		Status:       StateDone,
		Pending:      nil,
	})
	decoded, ok := DecodeState(body)
	if !ok || len(decoded.Pending) != 0 {
		t.Fatalf("decoded = %+v ok=%v, want ok with empty pending", decoded, ok)
	}
	if !HasState(body) {
		t.Fatal("HasState: want true")
	}
}

// A chunk id is a 12 character lowercase hex string, and Task 7 puts several
// of them in Pending. This proves every field, including both pending
// entries and their order, survives the round trip, not only the first
// entry the way TestStateMarkerRoundTrip checks it.
func TestStateMarkerRoundTripPreservesRealisticChunkIDsInOrder(t *testing.T) {
	original := State{
		LastReviewed: domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32"),
		RunID:        "delivery-realistic-chunks",
		Status:       StateReviewing,
		Pending:      []string{"a1b2c3d4e5f6", "0123456789ab"},
	}

	body := "## Review\n\nsummary prose\n\n" + EncodeState(original) + "\n"

	decoded, ok := DecodeState(body)
	if !ok {
		t.Fatal("DecodeState: marker not found")
	}
	if decoded.LastReviewed != original.LastReviewed {
		t.Fatalf("last reviewed = %q, want %q", decoded.LastReviewed, original.LastReviewed)
	}
	if decoded.RunID != original.RunID {
		t.Fatalf("run id = %q, want %q", decoded.RunID, original.RunID)
	}
	if decoded.Status != original.Status {
		t.Fatalf("status = %q, want %q", decoded.Status, original.Status)
	}
	if len(decoded.Pending) != len(original.Pending) {
		t.Fatalf("pending = %v, want %v", decoded.Pending, original.Pending)
	}
	for index, chunkID := range original.Pending {
		if decoded.Pending[index] != chunkID {
			t.Fatalf("pending[%d] = %q, want %q", index, decoded.Pending[index], chunkID)
		}
	}
}

// A pull request whose first run failed has a marker to find and no reviewed
// commit to name. The marker must still decode, or the next run cannot read
// the pending work recorded beside it.
func TestStateMarkerRoundTripsWithNoReviewedCommit(t *testing.T) {
	body := EncodeState(State{
		LastReviewed: domain.HeadSHA(""),
		RunID:        "delivery-abc",
		Status:       StateFailed,
		Pending:      []string{"a1b2c3d4e5f6"},
	})

	decoded, ok := DecodeState(body)
	if !ok {
		t.Fatalf("DecodeState(%q): want the marker decoded", body)
	}
	if decoded.LastReviewed != domain.HeadSHA("") {
		t.Fatalf("last reviewed = %q, want empty", decoded.LastReviewed)
	}
	if decoded.Status != StateFailed {
		t.Fatalf("status = %q, want %q", decoded.Status, StateFailed)
	}
	if len(decoded.Pending) != 1 || decoded.Pending[0] != "a1b2c3d4e5f6" {
		t.Fatalf("pending = %v, want the one chunk id", decoded.Pending)
	}
}

func TestStateMarkerRejectsMalformedHeadSHA(t *testing.T) {
	body := "<!-- pr-review-agent:state:v1 last_reviewed=zzzz run=delivery-abc status=reviewing pending= -->"
	if _, ok := DecodeState(body); ok {
		t.Fatal("malformed state marker accepted")
	}
}

// The completed list is what keeps the next run from paying for chunks an
// earlier one already read, so it has to survive the round trip in order and
// beside the pending list rather than in place of it.
func TestStateMarkerRoundTripsTheCompletedChunkIDs(t *testing.T) {
	original := State{
		LastReviewed: domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32"),
		RunID:        "delivery-abc",
		Status:       StateReviewing,
		Pending:      []string{"a1b2c3d4e5f6"},
		Completed:    []string{"0123456789ab", "fedcba987654"},
	}

	decoded, ok := DecodeState("## Review\n\nprose\n\n" + EncodeState(original) + "\n")
	if !ok {
		t.Fatal("DecodeState: marker not found")
	}
	if len(decoded.Pending) != 1 || decoded.Pending[0] != "a1b2c3d4e5f6" {
		t.Fatalf("pending = %v, want the one owed chunk", decoded.Pending)
	}
	if len(decoded.Completed) != 2 {
		t.Fatalf("completed = %v, want both ids", decoded.Completed)
	}
	for index, id := range original.Completed {
		if decoded.Completed[index] != id {
			t.Fatalf("completed[%d] = %q, want %q", index, decoded.Completed[index], id)
		}
	}
}

// A marker written before the completed list existed must still decode. Reading
// one as unparseable would throw away the pending list it does carry and send
// the next run over the whole pull request again.
func TestStateMarkerWithoutACompletedListStillDecodes(t *testing.T) {
	body := "<!-- pr-review-agent:state:v1 last_reviewed=a3c4f1cac7f595bc824704b9d2a1f1191630dc32 " +
		"run=delivery-abc status=reviewing pending=a1b2c3d4e5f6 -->"

	decoded, ok := DecodeState(body)
	if !ok {
		t.Fatalf("DecodeState(%q): want the older marker decoded", body)
	}
	if len(decoded.Pending) != 1 || decoded.Pending[0] != "a1b2c3d4e5f6" {
		t.Fatalf("pending = %v, want the id the older marker carried", decoded.Pending)
	}
	if len(decoded.Completed) != 0 {
		t.Fatalf("completed = %v, want none: the older marker recorded none", decoded.Completed)
	}
}

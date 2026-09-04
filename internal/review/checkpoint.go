package review

// This file owns the durable checkpoint: what the run records about how far it
// got, and the rule that decides when the last reviewed commit may advance.
//
// The checkpoint is the service's only memory. Everything it does not record is
// forgotten the moment the process ends, which is why the shortfall a chunk
// leaves behind is written here rather than kept in the pass that observed it.

import (
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

// concludeState closes the pass out. The last reviewed commit advances only
// when nothing is left pending, so a run that could not read the whole head
// never claims it did.
//
// Advancing it also drops the completed set and the recorded shortfall. Both
// exist to describe work under the current baseline; once the baseline moves,
// the next delta starts after them and every id in either names a chunk that can
// never appear again.
//
// unreadable says the delta holds something no run can read, such as a hunk
// larger than one model request. Nothing is pending in that case, because every
// chunk answered, so the baseline would otherwise advance over code nobody read
// and the next delta would start after it. Holding it keeps that code in every
// later delta, exactly as a declined delta stays in one. The completed set is
// kept for the same reason it is kept while chunks are pending: the chunks that
// did answer must not be paid for twice.
func concludeState(
	state marker.State,
	job domain.ReviewJob,
	head domain.HeadSHA,
	tracker *pendingTracker,
	unreadable bool,
) marker.State {
	unfinished := tracker.remaining()
	state.Pending = unfinished
	state.Completed = tracker.finished()
	state.Unread = tracker.unreadable()
	state.RunID = job.DeliveryID
	state.Status = marker.StateReviewing
	if unreadable {
		return state
	}
	if len(unfinished) == 0 {
		state.LastReviewed = head
		state.Status = marker.StateDone
		state.Completed = nil
		state.Unread = nil
	}
	return state
}

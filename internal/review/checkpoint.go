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

// completeUnreadChunks closes unread chunks only when they are all that remains.
// Other pending work keeps the full set for the next run to reconsider together.
func (tracker *pendingTracker) completeUnreadChunks() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	unread := make(map[string]struct{}, len(tracker.unread))
	for _, id := range tracker.unread {
		unread[id] = struct{}{}
	}
	unfinished := make(map[string]struct{}, len(tracker.unfinished))
	for _, id := range tracker.unfinished {
		unfinished[id] = struct{}{}
		if _, found := unread[id]; !found {
			return
		}
	}
	for id := range unread {
		if _, found := unfinished[id]; !found {
			return
		}
	}
	tracker.completed = append(tracker.completed, tracker.unfinished...)
	tracker.unfinished = nil
	tracker.unread = nil
}

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
// larger than one model request. Holding the baseline keeps that code in every
// later delta until the model decides it from the current pull request context.
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

package marker

import (
	"fmt"
	"regexp"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

// State statuses. Reviewing covers a run still working and a run that stopped
// with chunks still pending, which the pending list itself tells apart; a run
// that read every chunk it owed is done; an over budget delta is skipped; a run
// that stopped on something other than a chunk is failed.
const (
	StateReviewing = "reviewing"
	StateDone      = "done"
	StateSkipped   = "skipped"
	StateFailed    = "failed"
)

const statePrefix = "<!-- pr-review-agent:state:v1 "

// An empty last_reviewed is a real state: a pull request whose first run
// failed or is still going has a marker to find and no reviewed commit to
// name. Requiring a commit there would make the encoder write markers the
// decoder rejects, and the pending list inside them would be lost.
//
// The completed list and the forcing delivery are optional for the same reason:
// a marker written before either existed still decodes. Reading such a marker as
// unparseable would throw away the pending list it does carry and re-review the
// whole pull request.
var statePattern = regexp.MustCompile(
	`<!-- pr-review-agent:state:v1 last_reviewed=([0-9a-f]{40}|[0-9a-f]{64})? run=(\S+) ` +
		`status=(reviewing|done|skipped|failed) pending=(\S*)(?: completed=(\S*))?` +
		`(?: forced_by=(\S*))?(?: unread=(\S*))? -->`,
)

// State is the durable review position kept on the one top level comment. It
// is the only memory the service has, so a new invocation reads it to learn
// what has been reviewed and what remains.
type State struct {
	LastReviewed domain.HeadSHA
	RunID        string
	Status       string
	// Pending names the chunks the last run could not read. It is what the
	// pull request reports, and what keeps LastReviewed from advancing.
	Pending []string
	// Completed names the chunks already read since LastReviewed. The next run
	// subtracts it from the delta, so a chunk that answered is not paid for
	// twice while a chunk a new commit introduced is still reviewed. It is
	// meaningless once LastReviewed advances, and is dropped there.
	Completed []string
	// ForcedBy names the forced delivery that last cleared this state to review
	// the pull request from scratch.
	//
	// It is deliberately not RunID. RunID names whichever run wrote the marker
	// last, so any later delivery reviewing the same head overwrites it, and the
	// record that a forced delivery already did its clearing is gone. A resume of
	// that forced delivery would then clear the state a second time and pay for
	// every chunk again. This field is written only by the run that clears, so
	// nothing else moves it, and every other writer carries it forward untouched.
	ForcedBy string
	// Unread names the chunks this service could not get a whole answer about,
	// such as one holding a hunk larger than a single model request.
	//
	// It is durable for the same reason Pending is. Such a chunk still answers,
	// so it lands in Completed and the next run subtracts it from the delta and
	// never re-derives it. Without this field the shortfall would live only in
	// the memory of the run that saw it, and the run after that one would find
	// nothing pending and advance LastReviewed over code nobody has ever read.
	//
	// The ids are content digests, so this clears itself: once the author
	// rewrites that code its chunk hashes differently, the recorded id matches
	// nothing in the delta, and the baseline is free to advance again.
	Unread []string
}

// EncodeState renders the state as one HTML comment line.
func EncodeState(state State) string {
	return fmt.Sprintf(
		"%slast_reviewed=%s run=%s status=%s pending=%s completed=%s forced_by=%s unread=%s%s",
		statePrefix,
		state.LastReviewed,
		state.RunID,
		state.Status,
		strings.Join(state.Pending, ","),
		strings.Join(state.Completed, ","),
		state.ForcedBy,
		strings.Join(state.Unread, ","),
		markerSuffix,
	)
}

// DecodeState finds and parses the state marker anywhere in a comment body.
func DecodeState(body string) (State, bool) {
	matches := statePattern.FindStringSubmatch(body)
	if len(matches) != 8 {
		return emptyState(), false
	}
	head := domain.HeadSHA("")
	if matches[1] != "" {
		parsed, err := domain.ParseHeadSHA(matches[1])
		if err != nil {
			return emptyState(), false
		}
		head = parsed
	}
	return State{
		LastReviewed: head,
		RunID:        matches[2],
		Status:       matches[3],
		Pending:      splitIDs(matches[4]),
		Completed:    splitIDs(matches[5]),
		ForcedBy:     matches[6],
		Unread:       splitIDs(matches[7]),
	}, true
}

// splitIDs parses one comma separated id list, treating an absent or empty
// list as no ids rather than as one empty id.
func splitIDs(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func emptyState() State {
	return State{
		LastReviewed: "", RunID: "", Status: "", Pending: nil, Completed: nil, ForcedBy: "", Unread: nil,
	}
}

// HasState reports whether a comment body carries the state marker.
func HasState(body string) bool {
	return strings.Contains(body, statePrefix)
}

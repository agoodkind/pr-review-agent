package review

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

const streamTestHeadSHA = "0123456789abcdef0123456789abcdef01234567"

// recordingGitHub is a minimal GitHub double for the sink's own tests. It
// records every posted comment and lets a test force one specific post to
// fail, without needing a full HTTP fixture server for logic that lives
// entirely inside streamingSink.
type recordingGitHub struct {
	GitHub
	posted []githubapp.InlineComment
	// failOn simulates GitHub answering and definitely rejecting a comment, the
	// same shape the real client returns for any non-2xx response.
	failOn func(githubapp.InlineComment) bool
	// failAmbiguouslyOn simulates a failure that carries no confirmation either
	// way, such as a dropped connection, rather than an HTTP error GitHub sent.
	failAmbiguouslyOn func(githubapp.InlineComment) bool
	// duringPost runs while this comment is still being posted, which is the
	// only moment a test can act on a reservation that is made but not settled.
	duringPost func()
}

func (github *recordingGitHub) CreateReviewComment(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	_ int,
	_ domain.HeadSHA,
	comment githubapp.InlineComment,
) error {
	if during := github.duringPost; during != nil {
		github.duringPost = nil
		during()
	}
	if github.failOn != nil && github.failOn(comment) {
		return githubapp.APIError{StatusCode: http.StatusUnprocessableEntity, Message: "comment rejected"}
	}
	if github.failAmbiguouslyOn != nil && github.failAmbiguouslyOn(comment) {
		return errors.New("connection reset by peer")
	}
	github.posted = append(github.posted, comment)
	return nil
}

func (github *recordingGitHub) GetPullRequest(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	_ int,
) (githubapp.PullRequest, error) {
	return githubapp.PullRequest{Head: domain.HeadSHA(streamTestHeadSHA)}, nil
}

func streamTestJob() domain.ReviewJob {
	return domain.ReviewJob{
		PullRequestRef: domain.PullRequestRef{
			Repository: domain.Repository{Owner: "owner", Name: "repo"},
			Number:     7,
			Head:       domain.HeadSHA(streamTestHeadSHA),
		},
	}
}

// The tail slot goes to whichever finding is most important, not whichever
// chunk answered first.
//
// Before this fix, admit spent capacity the moment a finding arrived, so an
// earlier, less important finding could take the run's last slot and leave no
// room for a more severe one a later chunk reported.
func TestStreamingSinkGivesTheTailSlotToTheMoreImportantLaterArrival(t *testing.T) {
	ctx := context.Background()
	github := &recordingGitHub{}
	state := publicationState{
		historyIDs:     map[string]struct{}{},
		historyAnchors: map[string]struct{}{},
		capacity:       0,
		hasTailSlot:    true,
	}
	sink := newStreamingSink(github, streamTestJob(), domain.HeadSHA(streamTestHeadSHA), &state, time.Second)

	low := domain.Finding{
		Path: "a.go", StartLine: 2, EndLine: 2,
		Title: "Minor issue", Body: "A smaller defect.", Importance: 9,
	}
	high := domain.Finding{
		Path: "b.go", StartLine: 3, EndLine: 3,
		Title: "Severe defect", Body: "A worse defect that answers later.", Importance: 10,
	}

	sink.Publish(ctx, []domain.Finding{low})  // arrives first, becomes the finalist
	sink.Publish(ctx, []domain.Finding{high}) // arrives later, more important, bumps it
	if len(github.posted) != 0 {
		t.Fatalf("posted = %v, want none until Finalize: the tail slot is not decided yet", github.posted)
	}

	sink.Finalize(ctx)

	if len(github.posted) != 1 || github.posted[0].Path != "b.go" {
		t.Fatalf("posted = %v, want only the more important finding", github.posted)
	}
	objections := sink.Objections()
	if len(objections) != 1 || objections[0].Importance != 10 {
		t.Fatalf("objections = %+v, want the importance 10 finding only", objections)
	}
}

// A failed delivery must not permanently spend the slot it reserved: a
// different, later finding can still use it in the same run.
//
// Before this fix, admit recorded a finding as delivered the moment it was
// reserved, before the caller even attempted to post it. A rejected comment
// therefore both cost the run a slot no other finding could use, and hid the
// rejected finding from every future run, since it was already remembered as
// if it had been shown.
func TestStreamingSinkReleasesTheSlotWhenDeliveryFails(t *testing.T) {
	ctx := context.Background()
	failing := domain.Finding{
		Path: "a.go", StartLine: 2, EndLine: 2,
		Title: "Flaky delivery", Body: "This one never posts.", Importance: 9,
	}
	other := domain.Finding{
		Path: "b.go", StartLine: 3, EndLine: 3,
		Title: "Different defect", Body: "This one should still get the slot.", Importance: 8,
	}
	github := &recordingGitHub{
		failOn: func(comment githubapp.InlineComment) bool {
			return comment.Path == "a.go"
		},
	}
	state := publicationState{
		historyIDs:     map[string]struct{}{},
		historyAnchors: map[string]struct{}{},
		capacity:       1,
		hasTailSlot:    true,
	}
	sink := newStreamingSink(github, streamTestJob(), domain.HeadSHA(streamTestHeadSHA), &state, time.Second)

	sink.Publish(ctx, []domain.Finding{failing})
	if len(github.posted) != 0 {
		t.Fatalf("posted = %v, want none: delivery should have failed", github.posted)
	}
	if len(state.historyIDs) != 0 || len(state.historyAnchors) != 0 {
		t.Fatalf(
			"history ids = %v anchors = %v, want both empty: a failed delivery must not be remembered",
			state.historyIDs, state.historyAnchors,
		)
	}

	sink.Publish(ctx, []domain.Finding{other})
	if len(github.posted) != 1 || github.posted[0].Path != "b.go" {
		t.Fatalf("posted = %v, want the other finding, using the slot the failed delivery released", github.posted)
	}
}

// A duplicate finding that arrives while the first copy's comment is still
// posting must not be admitted a second time.
//
// Suppression alone cannot catch it: the sink records a finding as carried only
// once its comment reaches the page, so during that window the same defect
// reported by a second chunk looked new, took a second slot, and posted twice.
func TestStreamingSinkSkipsADuplicateArrivingWhileItsFirstCommentPosts(t *testing.T) {
	ctx := context.Background()
	duplicate := domain.Finding{
		Path: "a.go", StartLine: 2, EndLine: 2,
		Title: "The same defect", Body: "Two chunks report this one.", Importance: 9,
	}
	github := &recordingGitHub{}
	state := publicationState{
		historyIDs:     map[string]struct{}{},
		historyAnchors: map[string]struct{}{},
		capacity:       2,
		hasTailSlot:    false,
	}
	sink := newStreamingSink(github, streamTestJob(), domain.HeadSHA(streamTestHeadSHA), &state, time.Second)

	github.duringPost = func() {
		sink.Publish(ctx, []domain.Finding{duplicate})
	}
	sink.Publish(ctx, []domain.Finding{duplicate})

	if len(github.posted) != 1 {
		t.Fatalf("posted = %d comments, want 1: the duplicate must not post again", len(github.posted))
	}
	if objections := sink.Objections(); len(objections) != 1 {
		t.Fatalf("objections = %+v, want the finding once", objections)
	}
	if state.capacity != 1 {
		t.Fatalf("capacity = %d, want 1: the duplicate must not spend a second slot", state.capacity)
	}
}

// A candidate that finds no free slot is kept, not discarded.
//
// Capacity can read as zero only because another chunk's comment is still in
// flight. When that comment then fails, its slot comes back, and the candidate
// that arrived during the gap is the one that should use it.
func TestStreamingSinkPostsAnOverflowCandidateAfterASlotComesBack(t *testing.T) {
	ctx := context.Background()
	first := domain.Finding{
		Path: "a.go", StartLine: 2, EndLine: 2,
		Title: "Never posts", Body: "This delivery fails.", Importance: 9,
	}
	arrivesDuring := domain.Finding{
		Path: "b.go", StartLine: 3, EndLine: 3,
		Title: "Arrives with no slot", Body: "It should still be published.", Importance: 8,
	}
	github := &recordingGitHub{
		failOn: func(comment githubapp.InlineComment) bool {
			return comment.Path == "a.go"
		},
	}
	state := publicationState{
		historyIDs:     map[string]struct{}{},
		historyAnchors: map[string]struct{}{},
		capacity:       1,
		hasTailSlot:    false,
	}
	sink := newStreamingSink(github, streamTestJob(), domain.HeadSHA(streamTestHeadSHA), &state, time.Second)

	github.duringPost = func() {
		sink.Publish(ctx, []domain.Finding{arrivesDuring})
	}
	sink.Publish(ctx, []domain.Finding{first})
	if len(github.posted) != 0 {
		t.Fatalf("posted = %v, want none yet: the only delivery so far failed", github.posted)
	}

	sink.Finalize(ctx)

	if len(github.posted) != 1 || github.posted[0].Path != "b.go" {
		t.Fatalf("posted = %v, want the candidate that arrived while the slot was busy", github.posted)
	}
}

// One finding that cannot be rendered must not take the rest of its batch down
// with it. Every other finding in the batch is still posted.
func TestStreamingSinkPostsTheRestOfABatchWhenOneFindingCannotRender(t *testing.T) {
	ctx := context.Background()
	// An absolute path is rejected when the comment body is built, so this
	// finding fails to render while its neighbor renders normally.
	unrenderable := domain.Finding{
		Path: "/etc/passwd", StartLine: 2, EndLine: 2,
		Title: "Cannot render", Body: "Its path is rejected.", Importance: 9,
	}
	valid := domain.Finding{
		Path: "b.go", StartLine: 3, EndLine: 3,
		Title: "Renders normally", Body: "This one should reach the page.", Importance: 9,
	}
	github := &recordingGitHub{}
	state := publicationState{
		historyIDs:     map[string]struct{}{},
		historyAnchors: map[string]struct{}{},
		capacity:       2,
		hasTailSlot:    false,
	}
	sink := newStreamingSink(github, streamTestJob(), domain.HeadSHA(streamTestHeadSHA), &state, time.Second)

	sink.Publish(ctx, []domain.Finding{unrenderable, valid})

	if len(github.posted) != 1 || github.posted[0].Path != "b.go" {
		t.Fatalf("posted = %v, want the finding that rendered", github.posted)
	}
	posted, failed := sink.Delivery()
	if posted != 1 || failed != 1 {
		t.Fatalf("delivery = (%d posted, %d failed), want (1, 1)", posted, failed)
	}
}

// A candidate the overflow pass could not fit must stay in the pool, because a
// delivery in that same pass can fail and hand its slot back.
func TestStreamingSinkKeepsWaitingOverflowUntilNoSlotComesBack(t *testing.T) {
	ctx := context.Background()
	holdsTheSlot := domain.Finding{
		Path: "a.go", StartLine: 2, EndLine: 2,
		Title: "Holds the only slot", Body: "Its delivery fails.", Importance: 9,
	}
	firstWaiting := domain.Finding{
		Path: "b.go", StartLine: 3, EndLine: 3,
		Title: "Tried first", Body: "It fails too, so its slot comes back.", Importance: 9,
	}
	secondWaiting := domain.Finding{
		Path: "c.go", StartLine: 4, EndLine: 4,
		Title: "Waits its turn", Body: "It should get the slot the retry released.", Importance: 8,
	}
	github := &recordingGitHub{
		failOn: func(comment githubapp.InlineComment) bool {
			return comment.Path == "a.go" || comment.Path == "b.go"
		},
	}
	state := publicationState{
		historyIDs:     map[string]struct{}{},
		historyAnchors: map[string]struct{}{},
		capacity:       1,
		hasTailSlot:    false,
	}
	sink := newStreamingSink(github, streamTestJob(), domain.HeadSHA(streamTestHeadSHA), &state, time.Second)

	github.duringPost = func() {
		sink.Publish(ctx, []domain.Finding{firstWaiting, secondWaiting})
	}
	sink.Publish(ctx, []domain.Finding{holdsTheSlot})

	sink.Finalize(ctx)

	if len(github.posted) != 1 || github.posted[0].Path != "c.go" {
		t.Fatalf("posted = %v, want the candidate that was still waiting when the retry freed a slot", github.posted)
	}
}

// A slot a failed delivery releases must go to a finding that was already
// waiting, not to whichever candidate a later Publish call happens to bring.
//
// considerBatch spends returned capacity on its own batch's candidates without
// first checking the overflow pool. A finding waiting since an earlier batch
// could then lose a released slot to a newer, less important arrival.
func TestStreamingSinkGivesAReleasedSlotToAnAlreadyWaitingFinding(t *testing.T) {
	ctx := context.Background()
	heldSlot := domain.Finding{
		Path: "a.go", StartLine: 2, EndLine: 2,
		Title: "Holds the only slot", Body: "Its delivery fails.", Importance: 9,
	}
	waitingLonger := domain.Finding{
		Path: "b.go", StartLine: 3, EndLine: 3,
		Title: "Has been waiting", Body: "It arrived first and should win the released slot.", Importance: 7,
	}
	arrivesLater := domain.Finding{
		Path: "c.go", StartLine: 4, EndLine: 4,
		Title: "Arrives in the next batch", Body: "It should not jump the queue.", Importance: 6,
	}
	github := &recordingGitHub{
		failOn: func(comment githubapp.InlineComment) bool {
			return comment.Path == "a.go"
		},
	}
	state := publicationState{
		historyIDs:     map[string]struct{}{},
		historyAnchors: map[string]struct{}{},
		capacity:       1,
		hasTailSlot:    false,
	}
	sink := newStreamingSink(github, streamTestJob(), domain.HeadSHA(streamTestHeadSHA), &state, time.Second)

	sink.Publish(ctx, []domain.Finding{heldSlot, waitingLonger})
	if len(github.posted) != 0 {
		t.Fatalf("posted = %v, want none: the only delivery attempted so far failed", github.posted)
	}

	sink.Publish(ctx, []domain.Finding{arrivesLater})

	if len(github.posted) != 1 || github.posted[0].Path != "b.go" {
		t.Fatalf("posted = %v, want the finding that had been waiting longer", github.posted)
	}
}

// A duplicate finding within one chunk's own reported findings must not post
// twice. The claimed check alone catches a duplicate across two Publish
// calls, but not two copies inside a single findings slice, because pending
// only gains an entry as candidates are admitted below the check.
func TestStreamingSinkSkipsADuplicateWithinTheSameBatch(t *testing.T) {
	ctx := context.Background()
	duplicate := domain.Finding{
		Path: "a.go", StartLine: 2, EndLine: 2,
		Title: "Reported twice by the same chunk", Body: "One model answer, two copies.", Importance: 9,
	}
	github := &recordingGitHub{}
	state := publicationState{
		historyIDs:     map[string]struct{}{},
		historyAnchors: map[string]struct{}{},
		capacity:       5,
		hasTailSlot:    false,
	}
	sink := newStreamingSink(github, streamTestJob(), domain.HeadSHA(streamTestHeadSHA), &state, time.Second)

	sink.Publish(ctx, []domain.Finding{duplicate, duplicate})

	if len(github.posted) != 1 {
		t.Fatalf("posted = %d comments, want 1 for two copies of the same finding", len(github.posted))
	}
	if state.capacity != 4 {
		t.Fatalf("capacity = %d, want 4: only one slot should be spent", state.capacity)
	}
}

// A post whose error carries no definite HTTP rejection is treated as if it
// delivered: the slot stays spent and the finding is remembered, because
// assuming success costs less than a duplicate comment or an exceeded cap
// would if the post actually did reach GitHub before the error occurred.
func TestStreamingSinkTreatsAnAmbiguousPostFailureAsDelivered(t *testing.T) {
	ctx := context.Background()
	ambiguous := domain.Finding{
		Path: "a.go", StartLine: 2, EndLine: 2,
		Title: "Connection drops mid post", Body: "GitHub's answer never arrives.", Importance: 9,
	}
	other := domain.Finding{
		Path: "b.go", StartLine: 3, EndLine: 3,
		Title: "A different defect", Body: "Must not get the slot the ambiguous post held.", Importance: 8,
	}
	github := &recordingGitHub{
		failAmbiguouslyOn: func(comment githubapp.InlineComment) bool {
			return comment.Path == "a.go"
		},
	}
	state := publicationState{
		historyIDs:     map[string]struct{}{},
		historyAnchors: map[string]struct{}{},
		capacity:       1,
		hasTailSlot:    false,
	}
	sink := newStreamingSink(github, streamTestJob(), domain.HeadSHA(streamTestHeadSHA), &state, time.Second)

	sink.Publish(ctx, []domain.Finding{ambiguous})
	if len(state.historyIDs) == 0 && len(state.historyAnchors) == 0 {
		t.Fatal("the ambiguous finding was not remembered, want it treated as delivered")
	}

	sink.Publish(ctx, []domain.Finding{other})
	if state.capacity != 0 {
		t.Fatalf("capacity = %d, want 0: the ambiguous post's slot must stay spent", state.capacity)
	}
	if len(sink.overflow) != 1 || sink.overflow[0].finding.Path != "b.go" {
		t.Fatalf("overflow = %+v, want the other finding waiting, not admitted", sink.overflow)
	}
}

// A finding GitHub definitely rejected must not be retried within the same
// run. A later chunk reporting the identical defect wastes the slot it
// releases repeating a failure GitHub already answered, rather than that slot
// going to a different finding.
func TestStreamingSinkDoesNotRetryARejectedFindingWithinTheRun(t *testing.T) {
	ctx := context.Background()
	rejected := domain.Finding{
		Path: "a.go", StartLine: 2, EndLine: 2,
		Title: "GitHub refuses this one", Body: "It comes back from a second chunk too.", Importance: 9,
	}
	different := domain.Finding{
		Path: "b.go", StartLine: 3, EndLine: 3,
		Title: "A different defect", Body: "It should get the slot the rejection released.", Importance: 8,
	}
	github := &recordingGitHub{
		failOn: func(comment githubapp.InlineComment) bool {
			return comment.Path == "a.go"
		},
	}
	state := publicationState{
		historyIDs:     map[string]struct{}{},
		historyAnchors: map[string]struct{}{},
		capacity:       1,
		hasTailSlot:    false,
	}
	sink := newStreamingSink(github, streamTestJob(), domain.HeadSHA(streamTestHeadSHA), &state, time.Second)

	sink.Publish(ctx, []domain.Finding{rejected})
	sink.Publish(ctx, []domain.Finding{rejected, different})

	if len(github.posted) != 1 || github.posted[0].Path != "b.go" {
		t.Fatalf("posted = %v, want only the different finding, not a retry of the rejected one", github.posted)
	}
}

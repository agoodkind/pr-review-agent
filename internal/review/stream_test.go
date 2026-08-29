package review

import (
	"context"
	"errors"
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
	failOn func(githubapp.InlineComment) bool
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
		return errors.New("comment rejected")
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

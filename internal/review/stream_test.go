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
}

func (github *recordingGitHub) CreateReviewComment(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	_ int,
	_ domain.HeadSHA,
	comment githubapp.InlineComment,
) error {
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

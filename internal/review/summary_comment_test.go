package review

import (
	"context"
	"errors"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

const summaryCommentTestBotLogin = "test-review-agent[bot]"

// summaryCommentGitHub is a minimal GitHub double for the summary comment's
// own tests. It records issue comment calls without needing a full HTTP
// fixture server for logic that lives entirely inside upsertSummaryComment
// and readState.
type summaryCommentGitHub struct {
	GitHub
	comments []githubapp.IssueComment
	updates  int
}

func (github *summaryCommentGitHub) ListIssueComments(
	context.Context,
	int64,
	domain.Repository,
	int,
) ([]githubapp.IssueComment, error) {
	return github.comments, nil
}

func (github *summaryCommentGitHub) CreateIssueComment(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	_ int,
	body string,
) (githubapp.IssueComment, error) {
	created := githubapp.IssueComment{
		ID:     int64(len(github.comments) + 1),
		Author: summaryCommentTestBotLogin,
		Body:   body,
	}
	github.comments = append(github.comments, created)
	return created, nil
}

func (github *summaryCommentGitHub) UpdateIssueComment(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	commentID int64,
	body string,
) (githubapp.IssueComment, error) {
	github.updates++
	for index, comment := range github.comments {
		if comment.ID != commentID {
			continue
		}
		comment.Body = body
		github.comments[index] = comment
		return comment, nil
	}
	return githubapp.IssueComment{ID: 0, Author: "", Body: ""}, errors.New("comment not found")
}

func summaryCommentTestJob() domain.ReviewJob {
	return domain.ReviewJob{
		DeliveryID: "delivery-1",
		PullRequestRef: domain.PullRequestRef{
			Repository:     domain.Repository{Owner: "owner", Name: "repo"},
			Number:         7,
			InstallationID: 99,
			Head:           domain.HeadSHA(internalTestHeadSHA),
		},
	}
}

// The comment is the service's memory, so a repeat call must edit it in
// place rather than leaving a trail of comments behind, and a later read
// must recover the state the most recent call wrote.
func TestUpsertSummaryCommentCreatesOnceThenUpdatesInPlace(t *testing.T) {
	github := &summaryCommentGitHub{}
	service := &Service{github: github, botLogin: summaryCommentTestBotLogin}
	job := summaryCommentTestJob()

	first := summaryCommentContent{
		Prose: "## Review\n\nfirst summary",
		State: marker.State{
			LastReviewed: domain.HeadSHA(internalTestHeadSHA),
			RunID:        "delivery-1",
			Status:       marker.StateDone,
			Pending:      nil,
		},
	}
	if err := service.upsertSummaryComment(context.Background(), job, first); err != nil {
		t.Fatalf("first upsertSummaryComment: %v", err)
	}
	if len(github.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(github.comments))
	}
	firstID := github.comments[0].ID

	second := summaryCommentContent{
		Prose: "## Review\n\nsecond summary",
		State: marker.State{
			LastReviewed: domain.HeadSHA(internalTestHeadSHA),
			RunID:        "delivery-2",
			Status:       marker.StateDone,
			Pending:      nil,
		},
	}
	if err := service.upsertSummaryComment(context.Background(), job, second); err != nil {
		t.Fatalf("second upsertSummaryComment: %v", err)
	}
	if len(github.comments) != 1 {
		t.Fatalf("comments = %d, want still 1 after the second call", len(github.comments))
	}
	if github.comments[0].ID != firstID {
		t.Fatal("comment id changed, want the first comment updated in place")
	}
	if github.updates != 1 {
		t.Fatalf("updates = %d, want 1", github.updates)
	}
	if !strings.Contains(github.comments[0].Body, "second summary") {
		t.Fatalf("body = %q, want the second call's prose", github.comments[0].Body)
	}

	readBack, found, err := service.readState(context.Background(), job)
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if !found {
		t.Fatal("readState: want the state found")
	}
	if readBack.RunID != "delivery-2" {
		t.Fatalf("readState run id = %q, want delivery-2", readBack.RunID)
	}
}

// readState must skip a comment from another author, because only the
// service's own comment carries state it can trust.
func TestReadStateIgnoresAForeignComment(t *testing.T) {
	github := &summaryCommentGitHub{
		comments: []githubapp.IssueComment{{
			ID:     1,
			Author: "someone-else",
			Body: marker.EncodeState(marker.State{
				LastReviewed: domain.HeadSHA(internalTestHeadSHA),
				RunID:        "delivery-1",
				Status:       marker.StateDone,
				Pending:      nil,
			}),
		}},
	}
	service := &Service{github: github, botLogin: summaryCommentTestBotLogin}

	_, found, err := service.readState(context.Background(), summaryCommentTestJob())
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if found {
		t.Fatal("readState: want no state found for a foreign comment")
	}
}

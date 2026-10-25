package githubapp_test

import (
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func TestLatestDecisionReviewAtHeadPreservesNativeDecisionIdentity(t *testing.T) {
	const author = "review-agent[bot]"
	const firstHead = domain.HeadSHA("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	const secondHead = domain.HeadSHA("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	reviews := []githubapp.Review{
		{ID: 1, Author: author, CommitID: firstHead, State: "CHANGES_REQUESTED"},
		{ID: 2, Author: author, CommitID: secondHead, State: "CHANGES_REQUESTED"},
		{ID: 3, Author: author, CommitID: firstHead, State: "COMMENTED"},
		{ID: 4, Author: "another-reviewer", CommitID: firstHead, State: "APPROVED"},
		{ID: 5, Author: author, CommitID: secondHead, State: "DISMISSED"},
	}
	if got := githubapp.LatestDecisionReviewAtHead(reviews, author, firstHead); got != reviews[0] {
		t.Fatalf("The first head must retain its own rejection despite later comments and other heads: got %+v.", got)
	}
	if got := githubapp.LatestDecisionReviewAtHead(reviews, author, secondHead); got != reviews[4] {
		t.Fatalf("The second head must retain its latest dismissal: got %+v.", got)
	}
	if got := githubapp.LatestDecisionReview(reviews, author); got != reviews[4] {
		t.Fatalf("The global selector must retain its existing latest-decision behavior: got %+v.", got)
	}
	reviews = append(reviews, githubapp.Review{ID: 6, Author: author, CommitID: firstHead, State: "APPROVED"})
	if got := githubapp.LatestDecisionReviewAtHead(reviews, author, firstHead); got != reviews[5] {
		t.Fatalf("A later native decision on the first head must replace its earlier rejection: got %+v.", got)
	}
	reviews = append(reviews, githubapp.Review{ID: 7, Author: author, CommitID: firstHead, State: "DISMISSED"})
	if got := githubapp.LatestDecisionReviewAtHead(reviews, author, firstHead); got != reviews[6] {
		t.Fatalf("A same-head dismissal must prevent reuse of its earlier approval: got %+v.", got)
	}
	var empty githubapp.Review
	if got := githubapp.LatestDecisionReviewAtHead(reviews, "missing-author", firstHead); got != empty {
		t.Fatalf("An absent author must not select another author's decision: got %+v.", got)
	}
}

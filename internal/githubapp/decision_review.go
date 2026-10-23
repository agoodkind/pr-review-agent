package githubapp

import (
	"slices"

	"goodkind.io/pr-review-agent/internal/domain"
)

type decisionReviewState string

// LatestDecisionReview requires reviews ordered from oldest to newest.
func LatestDecisionReview(reviews []Review, author string) Review {
	return latestDecisionReview(reviews, author, "", false)
}

// LatestDecisionReviewAtHead selects a native decision at the exact head.
// It requires reviews ordered from oldest to newest.
func LatestDecisionReviewAtHead(reviews []Review, author string, head domain.HeadSHA) Review {
	return latestDecisionReview(reviews, author, head, true)
}

func latestDecisionReview(reviews []Review, author string, head domain.HeadSHA, onlyHead bool) Review {
	for _, review := range slices.Backward(reviews) {
		if review.Author != author {
			continue
		}
		if onlyHead && review.CommitID != head {
			continue
		}
		switch decisionReviewState(review.State) {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			return review
		}
	}
	var empty Review
	return empty
}

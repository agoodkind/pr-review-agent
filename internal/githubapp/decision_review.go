package githubapp

import "slices"

type decisionReviewState string

// LatestDecisionReview requires reviews ordered from oldest to newest.
func LatestDecisionReview(reviews []Review, author string) Review {
	for _, review := range slices.Backward(reviews) {
		if review.Author != author {
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

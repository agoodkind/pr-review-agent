package review

import (
	"errors"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func dismissedReviewBlocked(dismissal githubapp.ReviewDismissal, legacyBody string) (bool, error) {
	switch dismissal.OriginalDecision {
	case domain.ReviewDecisionRequestChanges:
		return true, nil
	case domain.ReviewDecisionApprove, domain.ReviewDecisionComment:
		return false, nil
	case "":
		if legacyBody != "" {
			return dismissedVerdictBlocked(legacyBody), nil
		}
		return false, errors.New("the original dismissed review decision is unavailable")
	default:
		return false, errors.New("the original dismissed review decision is invalid")
	}
}

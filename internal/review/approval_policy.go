package review

import (
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// SetApproveEachCommit configures approval after each clean head review.
func (service *Service) SetApproveEachCommit(approveEachCommit bool) {
	service.approveEachCommit = approveEachCommit
}

func nativeVerdictBody(summary Summary) string {
	if summary.Decision == domain.ReviewDecisionApprove {
		return ""
	}
	return RenderVerdictBody(summary)
}

func (service *Service) standingVerdict(job domain.ReviewJob, reviews []githubapp.Review, summary Summary) headVerdict {
	if summary.Decision == domain.ReviewDecisionApprove && !service.settingsFor(job).approveEachCommit {
		latest := githubapp.LatestDecisionReview(reviews, service.botLogin)
		if latest.State == reviewStateApproved {
			return headVerdict{review: latest, found: true, withdrawn: false}
		}
	}
	return latestBotVerdictAtHead(reviews, service.botLogin, summary.Head)
}

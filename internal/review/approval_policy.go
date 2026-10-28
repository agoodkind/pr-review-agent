package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// SetApproveEachCommit configures approval after each clean head review.
func (service *Service) SetApproveEachCommit(approveEachCommit bool) {
	service.approveEachCommit = approveEachCommit
}

func (service *Service) nativeVerdictBody(ctx context.Context, job domain.ReviewJob, summary Summary) (string, error) {
	if summary.Decision == domain.ReviewDecisionApprove {
		return "", nil
	}
	logger := gklog.L(ctx)
	comments, err := service.github.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "read native review summary", slog.String("err", err.Error()))
		return "", fmt.Errorf("read native review summary: %w", err)
	}
	comment, found := findSummaryComment(comments, service.botLogin)
	if !found {
		var state marker.State
		state.RunID, state.Status = job.DeliveryID, marker.StateDone
		if summary.CoverageComplete {
			state.LastReviewed = summary.Head
		}
		if err := service.upsertSummaryComment(ctx, job, summaryCommentContent{Prose: refreshVerdictProse("", summary, false), State: state}); err != nil {
			return "", err
		}
		comments, err = service.github.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
		if err != nil {
			logger.ErrorContext(ctx, "read recovered native review summary", slog.String("err", err.Error()))
			return "", fmt.Errorf("read recovered native review summary: %w", err)
		}
		comment, found = findSummaryComment(comments, service.botLogin)
	}
	if !found || comment.ID <= 0 {
		return "", errors.New("native review requires the bot summary comment after recovery")
	}
	if summary.Decision == domain.ReviewDecisionRequestChanges {
		return service.nativeRequestedChangesBody(ctx, job, summary, comment.ID)
	}
	return NativeReviewSummaryLink(service.reviewPolicy, job.Repository, job.Number, comment.ID, RenderVerdictBody(summary))
}

func nativeSummaryURL(repo domain.Repository, number int, commentID int64) string {
	return repositoryURL(repo) + "/pull/" + strconv.Itoa(number) + "#issuecomment-" + strconv.FormatInt(commentID, 10)
}

// NativeReviewSummaryLink appends checkpoint data after the visible summary permalink.
func NativeReviewSummaryLink(policy reviewrules.Policy, repo domain.Repository, number int, commentID int64, checkpoint string) (string, error) {
	if number <= 0 || commentID <= 0 || repo.Owner == "" || repo.Name == "" {
		return "", errors.New("native review summary link requires repository, pull request, and comment identifiers")
	}
	var data reviewrules.PromptData
	data.SummaryURL = nativeSummaryURL(repo, number, commentID)
	link, err := renderPrompt(policy, "native.summary_link", data)
	if err != nil {
		return "", err
	}
	if !strings.Contains(link, data.SummaryURL) || strings.TrimSpace(link) == "" {
		return "", errors.New("native.summary_link must include the summary permalink")
	}
	return link + "\n\n" + checkpoint, nil
}

func (service *Service) updateStandingVerdict(ctx context.Context, job domain.ReviewJob, standing githubapp.Review, summary Summary) error {
	if err := service.validateInlineVerdict(ctx, job, summary); err != nil {
		return err
	}
	if standing.CommitID != summary.Head {
		return nil
	}
	if standing.State == reviewStateChangesRequested && strings.TrimSpace(standing.Body) == "" {
		return nil
	}
	body, err := service.nativeVerdictBody(ctx, job, summary)
	if err != nil {
		return err
	}
	return service.updateVerdictBody(ctx, job, standing, body)
}

func (service *Service) standingVerdict(job domain.ReviewJob, reviews []githubapp.Review, summary Summary) headVerdict {
	latest := githubapp.LatestDecisionReview(reviews, service.botLogin)
	if summary.Decision == domain.ReviewDecisionRequestChanges && latest.State == reviewStateChangesRequested {
		return headVerdict{review: latest, found: true, withdrawn: false}
	}
	if summary.Decision == domain.ReviewDecisionApprove && !service.settingsFor(job).approveEachCommit {
		if latest.State == reviewStateApproved {
			return headVerdict{review: latest, found: true, withdrawn: false}
		}
	}
	return latestBotVerdictAtHead(reviews, service.botLogin, summary.Head)
}

func (service *Service) preserveDismissedRejection(ctx context.Context, job domain.ReviewJob, reviews []githubapp.Review) (bool, error) {
	logger := gklog.L(ctx)
	latest := githubapp.LatestDecisionReviewAtHead(reviews, service.botLogin, job.Head)
	if latest.State != reviewStateDismissed || latest.CommitID != job.Head {
		return false, nil
	}
	dismissal, found, err := service.github.FindReviewDismissal(ctx, job.InstallationID, job.Repository, job.Number, latest.ID)
	if err != nil {
		logger.WarnContext(ctx, "read dismissed rejection before publishing", "review_id", latest.ID, "err", err)
		return false, fmt.Errorf("read dismissed rejection before publishing: %w", err)
	}
	if !found {
		return false, errors.New("review dismissal event is not available")
	}
	blocked, err := dismissedReviewBlocked(dismissal, latest.Body)
	if err != nil {
		logger.WarnContext(ctx, "read original dismissed decision before publishing", "review_id", latest.ID, "err", err)
		return false, err
	}
	return blocked && !automaticFailureDismissal(dismissal, service.botLogin, latest.ID), nil
}

package review

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/queue"
)

const (
	checkSummarySuccess   = "Review complete."
	checkSummaryFailure   = "Review failed."
	checkSummaryCancelled = "Review cancelled."
	checkFailureReviews   = "Review failed while reading existing reviews."
	checkFailureReconcile = "Review failed while reconciling existing findings."
	checkFailureDiff      = "Review failed while collecting the pull request diff."
	checkFailureAnalysis  = "Review failed during model analysis."
	checkFailureRefresh   = "Review failed while refreshing the pull request head."
	checkFailureRender    = "Review failed while rendering inline findings."
	checkFailureSummary   = "Review failed while updating the visible summary."
	checkFailurePublish   = "Review failed while publishing the final decision."
)

// GitHub loads pull request state and publishes review lifecycle updates.
type GitHub interface {
	GetPullRequest(context.Context, int64, domain.Repository, int) (githubapp.PullRequest, error)
	ListReviews(context.Context, int64, domain.Repository, int) ([]githubapp.Review, error)
	FindCheckRun(context.Context, int64, domain.Repository, domain.HeadSHA, string) (githubapp.CheckRun, bool, error)
	CreateCheckRun(context.Context, int64, domain.Repository, domain.HeadSHA, string) (githubapp.CheckRun, error)
	StartCheckRun(context.Context, int64, domain.Repository, int64, string) error
	CompleteCheckRun(context.Context, int64, domain.Repository, int64, string, string) error
	SubmitReview(context.Context, int64, domain.Repository, int, githubapp.SubmitReviewRequest) (githubapp.Review, error)
	UpdateReview(context.Context, int64, domain.Repository, int, int64, string) (githubapp.Review, error)
}

// Collector gathers pull request diff input for one review pass.
type Collector interface {
	Collect(context.Context, domain.PullRequestRef, githubapp.PullRequest) (diff.ReviewInput, error)
}

// Reconciler silently resolves earlier bot findings on the current head.
type Reconciler interface {
	Reconcile(context.Context, domain.ReviewJob) ([]githubapp.ReviewThread, error)
}

// Service publishes one complete GitHub review per pull request head.
type Service struct {
	github                    GitHub
	collector                 Collector
	model                     Model
	reconciler                Reconciler
	locker                    *queue.KeyedLocker
	botLogin                  string
	checkName                 string
	minimumImportance         int
	maximumUnresolvedComments int
	logger                    *slog.Logger
}

// NewService constructs a review publication service.
func NewService(
	github GitHub,
	collector Collector,
	model Model,
	reconciler Reconciler,
	locker *queue.KeyedLocker,
	botLogin string,
	minimumImportance int,
	maximumUnresolvedComments int,
	logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		github:                    github,
		collector:                 collector,
		model:                     model,
		reconciler:                reconciler,
		locker:                    locker,
		botLogin:                  botLogin,
		checkName:                 config.ReviewCheckName,
		minimumImportance:         minimumImportance,
		maximumUnresolvedComments: maximumUnresolvedComments,
		logger:                    logger,
	}
}

// Run reviews one pull request head and publishes the GitHub review lifecycle.
func (service *Service) Run(parent context.Context, job domain.ReviewJob) error {
	ctx, cancel := context.WithTimeout(parent, config.ReviewTimeout)
	defer cancel()

	unlock := service.locker.Lock(job.Key())
	defer unlock()

	pullRequest, head, checkRun, err := service.prepare(ctx, job)
	if err != nil {
		return err
	}
	if checkRun.Status == "completed" && checkRun.Conclusion == "success" {
		return nil
	}

	reviews, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureReviews, err)
	}
	if hasBotReviewMarker(reviews, service.botLogin, head) {
		return service.succeed(ctx, job, checkRun.ID)
	}

	threads, err := service.reconciler.Reconcile(ctx, job)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureReconcile, err)
	}

	input, err := service.collector.Collect(ctx, job.PullRequestRef, pullRequest)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureDiff, err)
	}

	analysis, err := Analyze(ctx, service.model, input, service.minimumImportance)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureAnalysis, err)
	}

	currentPullRequest, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureRefresh, err)
	}
	if currentPullRequest.Head != head {
		return service.cancelCheck(ctx, job, checkRun.ID)
	}

	publishedFindings, err := selectFindingsForPublication(
		ctx,
		analysis.Anchored,
		reviews,
		threads,
		service.botLogin,
		service.maximumUnresolvedComments,
		service.logger,
	)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureRender, err)
	}

	comments, err := RenderInline(head, publishedFindings)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureRender, err)
	}

	body, err := service.prepareReviewBody(ctx, job, reviews, head, analysis.Decision)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureSummary, err)
	}

	_, err = service.github.SubmitReview(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
		githubapp.SubmitReviewRequest{
			CommitID: head,
			Body:     body,
			Event:    analysis.Decision,
			Comments: comments,
		},
	)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailurePublish, err)
	}

	return service.succeed(ctx, job, checkRun.ID)
}

func (service *Service) prepare(
	ctx context.Context,
	job domain.ReviewJob,
) (githubapp.PullRequest, domain.HeadSHA, githubapp.CheckRun, error) {
	pullRequest, err := service.github.GetPullRequest(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
	)
	if err != nil {
		service.logger.ErrorContext(ctx, "get pull request", slog.String("err", err.Error()))
		return githubapp.PullRequest{}, "", githubapp.CheckRun{}, fmt.Errorf("get pull request: %w", err)
	}

	checkRun, err := service.ensureCheckRun(ctx, job, pullRequest.Head)
	if err != nil {
		return githubapp.PullRequest{}, "", githubapp.CheckRun{}, err
	}

	return pullRequest, pullRequest.Head, checkRun, nil
}

func (service *Service) ensureCheckRun(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
) (githubapp.CheckRun, error) {
	checkRun, found, err := service.github.FindCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		head,
		service.checkName,
	)
	if err != nil {
		service.logger.ErrorContext(ctx, "find check run", slog.String("err", err.Error()))
		return githubapp.CheckRun{}, fmt.Errorf("find check run: %w", err)
	}
	if !found {
		checkRun, err = service.github.CreateCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			head,
			service.checkName,
		)
		if err != nil {
			service.logger.ErrorContext(ctx, "create check run", slog.String("err", err.Error()))
			return githubapp.CheckRun{}, fmt.Errorf("create check run: %w", err)
		}
	}
	if checkRun.Status == "queued" {
		if err := service.github.StartCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			checkRun.ID,
			"",
		); err != nil {
			service.logger.ErrorContext(ctx, "start check run", slog.String("err", err.Error()))
			return githubapp.CheckRun{}, fmt.Errorf("start check run: %w", err)
		}
	}
	return checkRun, nil
}

func (service *Service) succeed(ctx context.Context, job domain.ReviewJob, checkRunID int64) error {
	if err := service.github.CompleteCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		checkRunID,
		"success",
		checkSummarySuccess,
	); err != nil {
		service.logger.ErrorContext(ctx, "complete successful check run", slog.String("err", err.Error()))
		return fmt.Errorf("complete check run: %w", err)
	}
	return nil
}

func (service *Service) failCheck(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	summary string,
	cause error,
) error {
	if summary == "" {
		summary = checkSummaryFailure
	}
	if checkRunID != 0 {
		completeErr := service.github.CompleteCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			checkRunID,
			"failure",
			summary,
		)
		if completeErr != nil {
			service.logger.ErrorContext(ctx, "complete failed check run", slog.String("err", completeErr.Error()))
			return fmt.Errorf("complete check run: %w", completeErr)
		}
	}
	service.logger.ErrorContext(ctx, "review job failed", slog.String("err", cause.Error()))
	return cause
}

func (service *Service) prepareReviewBody(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
	head domain.HeadSHA,
	decision domain.ReviewDecision,
) (string, error) {
	summaryReview, found := findSummaryReview(reviews, service.botLogin)
	if decision == domain.ReviewDecisionApprove {
		if found && summaryReview.Body != marker.Summary() {
			if _, err := service.github.UpdateReview(
				ctx,
				job.InstallationID,
				job.Repository,
				job.Number,
				summaryReview.ID,
				marker.Summary(),
			); err != nil {
				service.logger.ErrorContext(ctx, "hide review summary", slog.String("err", err.Error()))
				return "", fmt.Errorf("hide review summary: %w", err)
			}
		}
		return marker.Review(head), nil
	}

	if found {
		if _, err := service.github.UpdateReview(
			ctx,
			job.InstallationID,
			job.Repository,
			job.Number,
			summaryReview.ID,
			RenderBody(head),
		); err != nil {
			service.logger.ErrorContext(ctx, "update review summary", slog.String("err", err.Error()))
			return "", fmt.Errorf("update review summary: %w", err)
		}
		return marker.Review(head), nil
	}
	return RenderBody(head), nil
}

func findSummaryReview(reviews []githubapp.Review, botLogin string) (githubapp.Review, bool) {
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		if marker.HasSummary(item.Body) {
			return item, true
		}
	}
	return githubapp.Review{
		ID:       0,
		CommitID: "",
		Author:   "",
		Body:     "",
		State:    "",
	}, false
}

func (service *Service) cancelCheck(ctx context.Context, job domain.ReviewJob, checkRunID int64) error {
	if err := service.github.CompleteCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		checkRunID,
		"cancelled",
		checkSummaryCancelled,
	); err != nil {
		service.logger.ErrorContext(ctx, "complete cancelled check run", slog.String("err", err.Error()))
		return fmt.Errorf("complete cancelled check run: %w", err)
	}
	return nil
}

func hasBotReviewMarker(
	reviews []githubapp.Review,
	botLogin string,
	head domain.HeadSHA,
) bool {
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		if item.CommitID != head {
			continue
		}
		markerHead, ok := marker.FindReview(item.Body)
		if !ok {
			continue
		}
		if markerHead == head {
			return true
		}
	}
	return false
}

func selectFindingsForPublication(
	ctx context.Context,
	findings []domain.Finding,
	reviews []githubapp.Review,
	threads []githubapp.ReviewThread,
	botLogin string,
	maximumUnresolvedComments int,
	logger *slog.Logger,
) ([]domain.Finding, error) {
	history := make(map[string]struct{})
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		findingMarker, ok := marker.FindFinding(item.Body)
		if ok {
			history[findingMarker.ID] = struct{}{}
		}
	}

	unresolvedCount := 0
	for _, thread := range threads {
		if thread.RootComment.Author != botLogin {
			continue
		}
		if !thread.Resolved {
			unresolvedCount++
		}
		findingMarker, ok := marker.FindFinding(thread.RootComment.Body)
		if ok {
			history[findingMarker.ID] = struct{}{}
		}
	}

	capacity := maximumUnresolvedComments - unresolvedCount
	if capacity <= 0 {
		return []domain.Finding{}, nil
	}

	selected := make([]domain.Finding, 0, len(findings))
	for _, finding := range findings {
		findingID, err := marker.FindingID(finding)
		if err != nil {
			logger.ErrorContext(ctx, "identify finding", slog.String("err", err.Error()))
			return nil, fmt.Errorf("identify finding: %w", err)
		}
		if _, exists := history[findingID]; exists {
			continue
		}
		selected = append(selected, finding)
	}

	sort.SliceStable(selected, func(left, right int) bool {
		if selected[left].Importance != selected[right].Importance {
			return selected[left].Importance > selected[right].Importance
		}
		return compareFindings(selected[left], selected[right]) < 0
	})
	if len(selected) > capacity {
		selected = selected[:capacity]
	}
	return selected, nil
}

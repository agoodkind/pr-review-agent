package review

import (
	"context"
	"fmt"
	"log/slog"

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
}

// Collector gathers pull request diff input for one review pass.
type Collector interface {
	Collect(context.Context, domain.PullRequestRef, githubapp.PullRequest) (diff.ReviewInput, error)
}

// Reconciler silently resolves earlier bot findings on the current head.
type Reconciler interface {
	Reconcile(context.Context, domain.ReviewJob) error
}

// Service publishes one complete GitHub review per pull request head.
type Service struct {
	github     GitHub
	collector  Collector
	model      Model
	reconciler Reconciler
	locker     *queue.KeyedLocker
	botLogin   string
	checkName  string
	logger     *slog.Logger
}

// NewService constructs a review publication service.
func NewService(
	github GitHub,
	collector Collector,
	model Model,
	reconciler Reconciler,
	locker *queue.KeyedLocker,
	botLogin string,
	logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		github:     github,
		collector:  collector,
		model:      model,
		reconciler: reconciler,
		locker:     locker,
		botLogin:   botLogin,
		checkName:  config.ReviewCheckName,
		logger:     logger,
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

	reviews, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, err)
	}
	if hasBotReviewMarker(reviews, service.botLogin, head) {
		return service.succeed(ctx, job, checkRun.ID)
	}

	input, err := service.collector.Collect(ctx, job.PullRequestRef, pullRequest)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, err)
	}

	analysis, err := Analyze(ctx, service.model, input)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, err)
	}

	currentPullRequest, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, err)
	}
	if currentPullRequest.Head != head {
		return service.cancelCheck(ctx, job, checkRun.ID)
	}

	comments, err := RenderInline(head, analysis.Anchored)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, err)
	}

	_, err = service.github.SubmitReview(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
		githubapp.SubmitReviewRequest{
			CommitID: head,
			Body:     RenderBody(head, analysis),
			Event:    analysis.Decision,
			Comments: comments,
		},
	)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, err)
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
	if err := service.reconciler.Reconcile(ctx, job); err != nil {
		service.logger.ErrorContext(
			ctx,
			"reconcile review findings",
			slog.String("err", err.Error()),
		)
	}
	return nil
}

func (service *Service) failCheck(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	cause error,
) error {
	if checkRunID != 0 {
		completeErr := service.github.CompleteCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			checkRunID,
			"failure",
			checkSummaryFailure,
		)
		if completeErr != nil {
			service.logger.ErrorContext(ctx, "complete failed check run", slog.String("err", completeErr.Error()))
			return fmt.Errorf("complete check run: %w", completeErr)
		}
	}
	service.logger.ErrorContext(ctx, "review job failed", slog.String("err", cause.Error()))
	return cause
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

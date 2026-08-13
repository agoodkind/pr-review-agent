package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/webhook"
)

func init() {
	keepInternalPackagesLinked()
}

// keepInternalPackagesLinked retains references until the HTTP runtime wires them in.
func keepInternalPackagesLinked() {
	_, _ = domain.ParseHeadSHA("")
	_, _ = domain.ParseReviewDecision("")
	_, _ = domain.ParseResolution("")
	_ = domain.PullRequestRef{}.Key()
	_ = domain.Finding{}.Validate()
	_ = domain.ReviewResult{}.Validate()
	_, _ = config.Load(func(string) (string, bool) { return "", false })
	_, _ = config.FromEnvironment()
	_ = marker.Review("")
	_, _ = marker.FindReview("")
	_, _ = marker.Finding("", domain.Finding{})
	_, _ = marker.FindFinding("")
	_, _ = marker.EncodeFindingBody("", domain.Finding{})
	_, _, _ = marker.DecodeFindingBody(domain.ReviewComment{})
	_, _ = marker.NormalizePath("")
	cache := queue.NewDeliveryCache(1, time.Second, time.Now)
	_ = cache.Claim
	_ = cache.Release
	_ = queue.NewKeyedLocker().Lock
	dispatcher := queue.NewDispatcher(1, noopRunner{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, runCancel := context.WithCancel(context.Background())
	dispatcher.Start(runCtx)
	_ = dispatcher.Enqueue(domain.ReviewJob{DeliveryID: "link-job"})
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Millisecond)
	_ = dispatcher.Shutdown(shutdownCtx)
	shutdownCancel()
	runCancel()
	_, _, _ = webhook.ParsePullRequest("", "", nil)
	_ = webhook.VerifySHA256("", nil, nil)
	_ = webhook.PullRequestEvent{}.Job()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, _ := config.Load(func(string) (string, bool) { return "", false })
	client := githubapp.NewClient(cfg, http.DefaultClient, time.Now, logger)
	_ = client
	_ = (*githubapp.Client)(nil).GetPullRequest
	_ = (*githubapp.Client)(nil).ListChangedFiles
	_ = (*githubapp.Client)(nil).GetFile
	_ = (*githubapp.Client)(nil).Compare
	_ = (*githubapp.Client)(nil).ListReviews
	_ = (*githubapp.Client)(nil).SubmitReview
	_ = (*githubapp.Client)(nil).FindCheckRun
	_ = (*githubapp.Client)(nil).CreateCheckRun
	_ = (*githubapp.Client)(nil).StartCheckRun
	_ = (*githubapp.Client)(nil).CompleteCheckRun
	_ = (*githubapp.Client)(nil).ListReviewThreads
	_ = (*githubapp.Client)(nil).ResolveReviewThread
	_ = githubapp.PullRequest{}
	_ = githubapp.ChangedFile{}
	_ = githubapp.Review{}
	_ = githubapp.InlineComment{}
	_ = githubapp.SubmitReviewRequest{}
	_ = githubapp.CheckRun{}
	_ = githubapp.ReviewThread{}
	var apiErr githubapp.APIError
	_ = apiErr.Error()

	collector := diff.NewCollector(noopDiffSource{})
	_, _ = collector.Collect(context.Background(), domain.PullRequestRef{}, githubapp.PullRequest{})
	_, _ = diff.ChangedRightLines("")
	_ = diff.ValidRange(nil, 1, 1)
	_, _ = diff.ChunkInput(diff.ReviewInput{}, 1)
	_ = diff.FileContext{}
	_ = diff.ReviewInput{}
	_ = diff.Chunk{}

	openaiClient := openai.NewClient(cfg, http.DefaultClient)
	_ = openaiClient
	_ = (*openai.Client)(nil).Review
	_ = (*openai.Client)(nil).Reconcile

	_ = review.DecisionFor(true, nil)
	_, _ = review.Analyze(context.Background(), reviewNoopModel{}, diff.ReviewInput{})
	_ = review.RenderBody(domain.HeadSHA(""), review.Analysis{})
	_, _ = review.RenderInline(domain.HeadSHA(""), nil)
	_ = review.UntrustedInputPolicy
	var reviewModel review.Model = openaiClient
	_ = reviewModel
	_ = review.Analysis{}
	reconciler := noopReconciler{}
	reviewService := review.NewService(
		client,
		collector,
		reviewNoopModel{},
		reconciler,
		queue.NewKeyedLocker(),
		cfg.GitHubBotLogin,
		logger,
	)
	_ = reviewService.Run
	var reviewGitHub review.GitHub = client
	_ = reviewGitHub
	var reviewCollector review.Collector = collector
	_ = reviewCollector
	var reviewReconciler review.Reconciler = reconciler
	_ = reviewReconciler
}

type reviewNoopModel struct{}

func (reviewNoopModel) Review(context.Context, string) (domain.ReviewResult, error) {
	return domain.ReviewResult{}, nil
}

type noopRunner struct{}

func (noopRunner) Run(context.Context, domain.ReviewJob) error { return nil }

type noopDiffSource struct{}

func (noopDiffSource) ListChangedFiles(
	context.Context,
	int64,
	domain.Repository,
	int,
) ([]githubapp.ChangedFile, error) {
	return nil, nil
}

func (noopDiffSource) GetFile(
	context.Context,
	int64,
	domain.Repository,
	string,
	domain.HeadSHA,
) ([]byte, error) {
	return nil, nil
}

type noopReconciler struct{}

func (noopReconciler) Reconcile(context.Context, domain.ReviewJob) error { return nil }

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/queue"
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
}

type noopRunner struct{}

func (noopRunner) Run(context.Context, domain.ReviewJob) error { return nil }

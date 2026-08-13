package main

import (
	"context"
	"io"
	"log/slog"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
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
}

type noopRunner struct{}

func (noopRunner) Run(context.Context, domain.ReviewJob) error { return nil }

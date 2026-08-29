// Command pr-review-agent runs end-to-end pull request reviews.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/gklog/correlation"
	"goodkind.io/pr-review-agent/internal/app"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/telemetry"
	"goodkind.io/pr-review-agent/internal/version"
)

const (
	githubHTTPTimeout = 30 * time.Second
	openaiHTTPTimeout = 610 * time.Second
	shutdownTimeout   = 30 * time.Second
)

func main() {
	slog.Debug("pr-review-agent start", slog.String("component", "pr-review-agent"))
	os.Exit(run(os.Args, os.Stdout, signal.NotifyContext))
}

func run(
	args []string,
	stdout io.Writer,
	notifyContext func(context.Context, ...os.Signal) (context.Context, context.CancelFunc),
) int {
	if len(args) > 1 {
		if args[1] == "--version" {
			_, _ = fmt.Fprintln(stdout, version.String())
			return 0
		}
		return 2
	}

	cfg, err := config.FromEnvironment()
	if err != nil {
		slog.Error("load config failed", slog.String("err", err.Error()))
		_, _ = fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}

	// Container stdout reaches no log sink, so a forwarder is added as a second
	// handler when one is configured. Every log the service already writes then
	// reaches a place a person can read, with no new call sites.
	//
	// Each handler is wrapped in correlation.SlogHandler so a record written
	// through a context carrying a correlation.Context gains request_id,
	// trace_id, and span_id, again with no new call sites.
	handlers := []slog.Handler{
		correlation.SlogHandler(
			slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo}),
			correlation.HandlerOptions{},
		),
	}
	var forwarder *telemetry.Forwarder
	if cfg.LogForwardURL != nil {
		forwarder = telemetry.NewForwarder(cfg.LogForwardURL.String(), cfg.GitHubWebhookSecret, nil)
		if forwarder != nil {
			handlers = append(handlers, correlation.SlogHandler(forwarder, correlation.HandlerOptions{}))
		}
	}

	logger, closer := gklog.New(gklog.Config{
		BuildVersion: version.String(),
		Handlers:     handlers,
	})
	defer func() {
		if closeErr := closer.Close(); closeErr != nil {
			logger.Error("close logger", slog.String("err", closeErr.Error()))
		}
		if closeErr := forwarder.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "close log forwarder: %v\n", closeErr)
		}
	}()
	slog.SetDefault(logger)
	logger.Info(
		"pr-review-agent configured",
		slog.String("version", version.String()),
		slog.String("platform", runtime.GOOS+"/"+runtime.GOARCH),
		slog.String("model", cfg.ReviewModel),
		slog.String("reasoning_effort", config.ReasoningEffort),
		slog.Bool("fallback_configured", cfg.HasFallback()),
		slog.Duration("review_timeout", cfg.ReviewTimeout),
		slog.Int("review_workers", cfg.ReviewWorkers),
		slog.Int("minimum_importance", cfg.MinimumImportance),
		slog.Int("maximum_unresolved_comments", cfg.MaximumUnresolvedComments),
		slog.String("github_bot_login", cfg.GitHubBotLogin),
	)
	githubHTTP := &http.Client{Timeout: githubHTTPTimeout}
	openaiHTTP := &http.Client{Timeout: openaiHTTPTimeout}
	application := app.New(cfg, githubHTTP, openaiHTTP, logger)

	ctx, stop := notifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	application.Start(ctx)

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := application.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown failed", slog.String("err", err.Error()))
		return 1
	}
	return 0
}

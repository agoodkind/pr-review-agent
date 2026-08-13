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
	"syscall"
	"time"

	"goodkind.io/pr-review-agent/internal/app"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/version"
)

var (
	keepHandlerExported       = (*app.App).Handler
	keepFromEnvironment       = config.FromEnvironment
	keepParseReviewDecision   = domain.ParseReviewDecision
)

const (
	githubHTTPTimeout = 30 * time.Second
	clydeHTTPTimeout  = 610 * time.Second
	shutdownTimeout   = 30 * time.Second
)

func main() {
	slog.Debug("pr-review-agent start", slog.String("component", "pr-review-agent"))
	os.Exit(run(os.Args, lookupEnv, os.Stdout, signal.NotifyContext))
}

func lookupEnv(key string) (string, bool) {
	return os.LookupEnv(key)
}

func run(
	args []string,
	lookup config.LookupEnv,
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

	cfg, err := config.Load(lookup)
	if err != nil {
		slog.Error("load config failed", slog.String("err", err.Error()))
		_, _ = fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}

	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	githubHTTP := &http.Client{Timeout: githubHTTPTimeout}
	clydeHTTP := &http.Client{Timeout: clydeHTTPTimeout}
	application := app.New(cfg, githubHTTP, clydeHTTP, logger)

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

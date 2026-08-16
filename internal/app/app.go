// Package app serves GitHub webhooks and runs the review job queue.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/reconcile"
	"goodkind.io/pr-review-agent/internal/review"
)

const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 60 * time.Second
	maxHeaderBytes    = 1 << 20
)

// App runs the HTTP webhook service and review job dispatcher.
type App struct {
	cfg        config.Config
	logger     *slog.Logger
	server     *http.Server
	dispatcher *queue.Dispatcher
	runCancel  context.CancelFunc
}

// New wires the review runtime and returns a ready application.
func New(cfg config.Config, githubHTTP *http.Client, openaiHTTP *http.Client, logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}
	if githubHTTP == nil {
		githubHTTP = http.DefaultClient
	}
	if openaiHTTP == nil {
		openaiHTTP = http.DefaultClient
	}

	githubClient := githubapp.NewClient(cfg, githubHTTP, time.Now, logger)
	openaiClient := openai.NewClient(cfg, openaiHTTP)
	reconcileService := reconcile.NewService(githubClient, openaiClient, cfg.GitHubBotLogin, logger)
	collector := diff.NewCollector(githubClient)
	reviewService := review.NewService(
		githubClient,
		collector,
		openaiClient,
		reconcileService,
		queue.NewKeyedLocker(),
		cfg.GitHubBotLogin,
		cfg.MinimumImportance,
		cfg.MaximumUnresolvedComments,
		cfg.ReviewTimeout,
		logger,
	)

	cache := queue.NewDeliveryCache(
		config.DeliveryCacheCapacity,
		config.DeliveryCacheTTL,
		time.Now,
	)
	dispatcher := queue.NewDispatcher(
		config.QueueCapacity,
		cfg.ReviewWorkers,
		reviewRunner{service: reviewService},
		logger,
	)
	httpHandler := newHandler(cfg, cache, dispatcher, reviewService, logger)

	return &App{
		cfg:        cfg,
		logger:     logger,
		dispatcher: dispatcher,
		runCancel:  nil,
		server: &http.Server{
			Addr:              ":" + cfg.Port,
			Handler:           httpHandler,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
			MaxHeaderBytes:    maxHeaderBytes,
		},
	}
}

// Start launches the dispatcher and HTTP server until the context is cancelled.
func (application *App) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	application.runCancel = cancel
	application.dispatcher.Start(runCtx)

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				application.logger.ErrorContext(
					runCtx,
					"http server panicked",
					slog.Any("panic", recovered),
					slog.String("err", "http server panicked"),
				)
			}
		}()
		application.logger.InfoContext(runCtx, "http server listening", slog.String("addr", application.server.Addr))
		err := application.server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			application.logger.ErrorContext(runCtx, "http server failed", slog.String("err", err.Error()))
		}
	}()
}

// Shutdown stops HTTP admission and drains accepted review jobs.
func (application *App) Shutdown(ctx context.Context) error {
	var shutdownErr error
	if application.server != nil {
		if err := application.server.Shutdown(ctx); err != nil {
			shutdownErr = fmt.Errorf("shutdown http server: %w", err)
		}
	}
	if application.runCancel != nil {
		application.runCancel()
	}
	if err := application.dispatcher.Shutdown(ctx); err != nil && shutdownErr == nil {
		shutdownErr = fmt.Errorf("shutdown dispatcher: %w", err)
	}
	return shutdownErr
}

type reviewRunner struct {
	service *review.Service
}

func (runner reviewRunner) Run(ctx context.Context, job domain.ReviewJob) error {
	logger := gklog.L(ctx)
	if err := runner.service.Run(ctx, job); err != nil {
		logger.ErrorContext(ctx, "review job", slog.String("err", err.Error()))
		return fmt.Errorf("review job: %w", err)
	}
	return nil
}

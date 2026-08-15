package queue

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"goodkind.io/pr-review-agent/internal/domain"
)

// Runner executes one review job.
type Runner interface {
	Run(context.Context, domain.ReviewJob) error
}

// Dispatcher runs review jobs on a single worker goroutine.
type Dispatcher struct {
	capacity int
	runner   Runner
	logger   *slog.Logger
	jobs     chan domain.ReviewJob
	started  bool
	mu       sync.Mutex
	wg       sync.WaitGroup
}

// NewDispatcher creates a bounded single-worker dispatcher.
func NewDispatcher(capacity int, runner Runner, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		capacity: capacity,
		runner:   runner,
		logger:   logger,
		jobs:     make(chan domain.ReviewJob, capacity),
		started:  false,
		mu:       sync.Mutex{},
		wg:       sync.WaitGroup{},
	}
}

// Start launches the worker loop until the context is cancelled.
func (dispatcher *Dispatcher) Start(ctx context.Context) {
	dispatcher.mu.Lock()
	if dispatcher.started {
		dispatcher.mu.Unlock()
		return
	}
	dispatcher.started = true
	dispatcher.mu.Unlock()

	dispatcher.wg.Go(func() {
		for job := range dispatcher.jobs {
			dispatcher.runJob(ctx, job)
		}
	})
}

func (dispatcher *Dispatcher) runJob(ctx context.Context, job domain.ReviewJob) {
	defer func() {
		if recovered := recover(); recovered != nil {
			dispatcher.logger.ErrorContext(
				ctx,
				"dispatcher job panicked",
				slog.Any("panic", recovered),
				slog.String("err", "dispatcher job panicked"),
			)
		}
	}()
	if err := dispatcher.runner.Run(ctx, job); err != nil {
		dispatcher.logger.ErrorContext(
			ctx,
			"review job failed",
			slog.String("error", err.Error()),
			slog.String("err", err.Error()),
		)
	}
}

// Enqueue adds a job without blocking. It returns false when the queue is full.
func (dispatcher *Dispatcher) Enqueue(job domain.ReviewJob) bool {
	select {
	case dispatcher.jobs <- job:
		return true
	default:
		return false
	}
}

// Shutdown waits for accepted jobs to finish or until the context is cancelled.
func (dispatcher *Dispatcher) Shutdown(ctx context.Context) error {
	close(dispatcher.jobs)

	done := make(chan struct{})
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				dispatcher.logger.ErrorContext(
					ctx,
					"dispatcher shutdown panicked",
					slog.Any("panic", recovered),
					slog.String("err", "dispatcher shutdown panicked"),
				)
			}
		}()
		dispatcher.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New(ctx.Err().Error())
	}
}

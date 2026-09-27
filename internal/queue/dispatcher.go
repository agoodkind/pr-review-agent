package queue

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"goodkind.io/pr-review-agent/internal/domain"
)

// Runner executes one review job.
type Runner interface {
	Run(context.Context, domain.ReviewJob) error
}

// Dispatcher runs review jobs on a bounded worker pool.
type Dispatcher struct {
	capacity  int
	workers   int
	runner    Runner
	logger    *slog.Logger
	jobs      chan domain.ReviewJob
	completed chan string
	started   bool
	closed    bool
	queued    int
	mu        sync.Mutex
	wg        sync.WaitGroup
}

// NewDispatcher creates a bounded worker pool.
func NewDispatcher(capacity int, workers int, runner Runner, logger *slog.Logger) *Dispatcher {
	return &Dispatcher{
		capacity:  capacity,
		workers:   workers,
		runner:    runner,
		logger:    logger,
		jobs:      make(chan domain.ReviewJob, capacity),
		completed: make(chan string, workers),
		started:   false,
		closed:    false,
		queued:    0,
		mu:        sync.Mutex{},
		wg:        sync.WaitGroup{},
	}
}

// Start launches the keyed scheduler.
func (dispatcher *Dispatcher) Start(ctx context.Context) {
	dispatcher.mu.Lock()
	if dispatcher.started || dispatcher.closed {
		dispatcher.mu.Unlock()
		return
	}
	dispatcher.started = true
	dispatcher.wg.Go(func() {
		dispatcher.schedule(ctx)
	})
	dispatcher.mu.Unlock()
}

func (dispatcher *Dispatcher) schedule(ctx context.Context) {
	jobs := dispatcher.jobs
	pending := make([]domain.ReviewJob, 0)
	activeKeys := make(map[string]struct{})
	running := 0

	for jobs != nil || running > 0 || len(pending) > 0 {
		for running < dispatcher.workers {
			job, remaining, ok := nextRunnableJob(pending, activeKeys)
			if !ok {
				break
			}
			pending = remaining
			key := job.Key()
			activeKeys[key] = struct{}{}
			running++
			dispatcher.markStarted()
			dispatcher.wg.Go(func() {
				dispatcher.runJob(ctx, job)
				dispatcher.completed <- key
			})
		}

		if jobs == nil && running == 0 {
			return
		}
		select {
		case job, ok := <-jobs:
			if !ok {
				jobs = nil
				continue
			}
			pending = append(pending, job)
		case key := <-dispatcher.completed:
			delete(activeKeys, key)
			running--
		}
	}
}

func nextRunnableJob(
	pending []domain.ReviewJob,
	activeKeys map[string]struct{},
) (domain.ReviewJob, []domain.ReviewJob, bool) {
	for index, job := range pending {
		if _, active := activeKeys[job.Key()]; active {
			continue
		}
		pending = append(pending[:index], pending[index+1:]...)
		return job, pending, true
	}
	var empty domain.ReviewJob
	return empty, pending, false
}

func (dispatcher *Dispatcher) markStarted() {
	dispatcher.mu.Lock()
	dispatcher.queued--
	dispatcher.mu.Unlock()
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
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	if dispatcher.closed || dispatcher.queued >= dispatcher.capacity {
		return false
	}

	select {
	case dispatcher.jobs <- job:
		dispatcher.queued++
		return true
	default:
		return false
	}
}

// Shutdown waits for accepted jobs to finish or until the context is cancelled.
func (dispatcher *Dispatcher) Shutdown(ctx context.Context) error {
	dispatcher.mu.Lock()
	if !dispatcher.closed {
		dispatcher.closed = true
		close(dispatcher.jobs)
	}
	dispatcher.mu.Unlock()

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
		return fmt.Errorf("dispatcher shutdown: %w", ctx.Err())
	}
}

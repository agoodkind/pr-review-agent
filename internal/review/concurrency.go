package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
)

// chunkOutcome is one chunk's result, kept separate per chunk so concurrent
// calls never share mutable state. The caller folds outcomes back in chunk
// order, which keeps deduplication and finding order identical to a serial run.
type chunkOutcome struct {
	chunk    int
	results  []domain.ReviewResult
	models   []string
	requests int
	err      error
	// panicked holds a panic raised inside this chunk's goroutine. An
	// unrecovered panic in any goroutine ends the whole process, so the
	// goroutine recovers and the caller raises it again on its own stack, where
	// the review's existing handler reports it as an internal panic.
	panicked string
}

// reviewChunksConcurrently reviews every chunk with bounded concurrency and
// returns one outcome per chunk, in chunk order.
//
// A review has a single time budget for the whole diff. Reviewing chunks one at
// a time spends that budget serially, so a diff large enough to need many
// chunks exhausts it no matter how fast each call is. Running a bounded number
// together spends the same budget in fewer waves.
//
// One chunk failing never stops the others. A chunk covers its own slice of the
// diff, so the findings in the rest stay valid and worth publishing. Cancelling
// them would throw away work that is already correct and leave the pull request
// with nothing, which is the worse outcome for the person waiting on a review.
func reviewChunksConcurrently(
	ctx context.Context,
	model Model,
	chunks []diff.Chunk,
	minimumImportance int,
	now func() time.Time,
	stream chunkStream,
) []chunkOutcome {
	outcomes := make([]chunkOutcome, len(chunks))
	limit := min(config.MaximumChunkConcurrency, len(chunks))
	if limit < 1 {
		return outcomes
	}

	slots := make(chan struct{}, limit)
	var waitGroup sync.WaitGroup
	for index, chunk := range chunks {
		waitGroup.Go(func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					gklog.L(ctx).ErrorContext(
						ctx,
						"review chunk panicked",
						slog.Int("chunk", chunk.Index),
						slog.Any("panic", recovered),
						slog.String("err", "review chunk panicked"),
					)
					outcomes[index] = chunkOutcome{
						chunk:    chunk.Index,
						results:  nil,
						models:   nil,
						requests: 0,
						err:      nil,
						panicked: fmt.Sprint(recovered),
					}
				}
			}()
			slots <- struct{}{}
			defer func() { <-slots }()
			outcomes[index] = reviewOneChunk(ctx, model, chunk, minimumImportance, now)
			stream.publish(ctx, outcomes[index])
		})
	}
	waitGroup.Wait()

	// A recovered panic becomes a typed error so the review reports it as an
	// internal panic rather than as an ordinary model failure.
	for index, outcome := range outcomes {
		if outcome.panicked != "" {
			outcomes[index].err = &chunkPanicError{chunk: outcome.chunk, value: outcome.panicked}
		}
	}
	return outcomes
}

// chunkStream sends one chunk's findings to the pull request as soon as that
// chunk answers, so the reader sees them without waiting for the rest.
type chunkStream struct {
	sink              FindingSink
	fileIndex         map[string]diff.FileContext
	minimumImportance int
}

// publish posts the eligible findings from one chunk. A chunk that failed has
// none, so nothing is posted for it.
func (stream chunkStream) publish(ctx context.Context, outcome chunkOutcome) {
	if stream.sink == nil || outcome.err != nil {
		return
	}
	findings := make([]domain.Finding, 0)
	for _, result := range outcome.results {
		findings = append(findings, result.Findings...)
	}
	eligible := eligibleFindings(findings, stream.fileIndex, stream.minimumImportance)
	if len(eligible) == 0 {
		return
	}
	stream.sink.Publish(ctx, eligible)
}

// chunkPanicError marks a chunk that panicked, so the caller can report the
// internal panic instead of blaming the model.
type chunkPanicError struct {
	chunk int
	value string
}

func (err *chunkPanicError) Error() string {
	return fmt.Sprintf("review chunk %d panicked: %s", err.chunk, err.value)
}

// isChunkPanic reports whether a review failure came from a panic inside a
// chunk rather than from the model or the provider.
func isChunkPanic(err error) bool {
	var target *chunkPanicError
	return errors.As(err, &target)
}

// reviewOneChunk adapts reviewChunk to per chunk accounting, so each goroutine
// writes only into its own outcome.
func reviewOneChunk(
	ctx context.Context,
	model Model,
	chunk diff.Chunk,
	minimumImportance int,
	now func() time.Time,
) chunkOutcome {
	var models modelSet
	requests := 0
	results, err := reviewChunk(ctx, model, chunk, minimumImportance, &models, &requests, now)
	return chunkOutcome{
		chunk:    chunk.Index,
		results:  results,
		models:   models.names,
		requests: requests,
		err:      err,
		panicked: "",
	}
}

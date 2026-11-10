package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

type chunkReviewAttempt struct {
	model             Model
	chunk             diff.Chunk
	minimumImportance int
	policy            reviewrules.Policy
	maximumBytes      int
	files             []diff.FileContext
	disputes          string
	models            *modelSet
	requests          *int
	now               func() time.Time
	observe           func(diff.Chunk, domain.ReviewResult) error
	required          string
	prompt            string
	elapsed           time.Duration
}

func (attempt *chunkReviewAttempt) complete(
	ctx context.Context,
	completion Completion,
	logger *slog.Logger,
) (chunkAnalysis, error) {
	nothing := chunkAnalysis{Results: nil, Unreadable: nil}

	var err error

	if validateErr := completion.Result.Validate(); validateErr != nil {
		logger.ErrorContext(ctx, "validate review result", slog.String("err", validateErr.Error()))

		return nothing, fmt.Errorf("validate review result: %w", validateErr)
	}

	attempt.models.add(completion.Model)
	logger.InfoContext(
		ctx,
		"review chunk completed",
		slog.Int("chunk", attempt.chunk.Index),
		slog.Int("chunks", attempt.chunk.Total),
		slog.Duration("elapsed", attempt.elapsed),
		slog.String("model", completion.Model),
		slog.Int("findings", len(completion.Result.Findings)),
		slog.Int("paths", len(attempt.chunk.Paths)),
		slog.Int("prompt_bytes", len(attempt.prompt)),
	)

	analysis := chunkAnalysis{
		Results:    []domain.ReviewResult{completion.Result},
		Unreadable: nil,
	}
	if attempt.observe != nil {
		err = attempt.observe(attempt.chunk, completion.Result)
	}

	if err != nil || len(completion.InspectionRequests) == 0 {
		return analysis, err
	}

	support, err := inspectReviewEvidence(
		ctx,
		completion.InspectionRequests,
		attempt.required,
		attempt.policy,
	)
	if err != nil {
		return analysis, err
	}

	continued, err := reviewChunkObserved(
		ctx,
		attempt.model,
		attempt.chunk,
		attempt.minimumImportance,
		attempt.policy,
		attempt.maximumBytes,
		attempt.files,
		attempt.disputes,
		attempt.models,
		attempt.requests,
		attempt.now,
		attempt.observe,
		support,
	)
	analysis.merge(continued)

	return analysis, err
}

func (attempt *chunkReviewAttempt) retry(
	ctx context.Context,
	err error,
	logger *slog.Logger,
) (chunkAnalysis, error) {
	nothing := chunkAnalysis{Results: nil, Unreadable: nil}

	// Every request that failed is timed here, before the truncation branch
	// decides what to do about it. A truncated request still spent its duration
	// on the review budget, and the split and skip logs below carry neither the
	// duration nor the size that caused the split.
	logger.LogAttrs(
		ctx,
		requestFailureLevel(err),
		"review chunk request failed",
		slog.Int("chunk", attempt.chunk.Index),
		slog.Int("chunks", attempt.chunk.Total),
		slog.Duration("elapsed", attempt.elapsed),
		slog.Bool("truncated", truncated(err)),
		slog.Any("paths", attempt.chunk.Paths),
		slog.Int("prompt_bytes", len(attempt.prompt)),
		slog.String("err", err.Error()),
	)

	var sizeFailure *diff.PromptSizeError
	if !truncated(err) && !errors.As(err, &sizeFailure) {
		return nothing, fmt.Errorf(
			"review chunk %d/%d: %w",
			attempt.chunk.Index,
			attempt.chunk.Total,
			err,
		)
	}

	first, second, canSplit := attempt.chunk.Split()
	if !canSplit {
		logger.WarnContext(
			ctx,
			"review chunk skipped after truncation",
			slog.Int("chunk", attempt.chunk.Index),
			slog.Any("paths", attempt.chunk.Paths),
			slog.String("err", err.Error()),
		)

		return chunkAnalysis{Results: nil, Unreadable: unreadableHunksIn(attempt.chunk)}, nil
	}

	logger.InfoContext(
		ctx,
		"review chunk split after truncation",
		slog.Int("chunk", attempt.chunk.Index),
		slog.Int("hunks", len(attempt.chunk.Pieces)),
	)

	return reviewChunkParts(
		ctx,
		attempt.model,
		[]diff.Chunk{first, second},
		attempt.minimumImportance,
		attempt.policy,
		attempt.maximumBytes,
		attempt.files,
		attempt.disputes,
		attempt.models,
		attempt.requests,
		attempt.now,
		attempt.observe,
		attempt.required,
	)
}

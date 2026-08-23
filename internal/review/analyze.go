package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

// Analyze reviews every deterministic chunk and aggregates the model output.
//
// The now function supplies the clock used for per-chunk timing, so a test can
// prove the timing without waiting for real model calls.
func Analyze(
	ctx context.Context,
	model Model,
	input diff.ReviewInput,
	minimumImportance int,
	now func() time.Time,
) (Analysis, error) {
	logger := gklog.L(ctx)
	// partial reports how far the analysis got. Every error path returns it, so
	// a failed review can still say which model answered and how much it read.
	partial := Analysis{
		CoverageComplete: false,
		Observed:         nil,
		Anchored:         nil,
		Decision:         "",
		FilesReviewed:    len(input.Files),
		Chunks:           0,
		Models:           nil,
	}

	chunks, err := diff.ChunkInput(input, config.MaximumPromptBytes)
	if err != nil {
		logger.ErrorContext(ctx, "chunk input", slog.String("err", err.Error()))
		return partial, fmt.Errorf("chunk input: %w", err)
	}
	partial.Chunks = len(chunks)

	coverageComplete := inputCoverageComplete(input.Files)
	for _, chunk := range chunks {
		if !chunk.CoverageComplete {
			coverageComplete = false
			break
		}
	}

	collector := newFindingCollector(input.Files, minimumImportance)
	analysisStartedAt := now()
	logger.InfoContext(ctx, "review model analysis started", slog.Int("chunks", len(chunks)))

	outcomes := reviewChunksConcurrently(ctx, model, chunks, minimumImportance, now)

	// Outcomes fold back in chunk order even though the calls ran together, so
	// deduplication and finding order stay identical to a serial review.
	var models modelSet
	requests := 0
	for _, outcome := range outcomes {
		requests += outcome.requests
		for _, name := range outcome.models {
			models.add(name)
		}
		if outcome.err != nil {
			// The elapsed total says whether the budget ran out or one call
			// failed early, which the failing chunk number alone cannot.
			logger.ErrorContext(
				ctx,
				"review model analysis failed",
				slog.Int("chunk", outcome.chunk),
				slog.Int("chunks", len(chunks)),
				slog.Int("model_requests", requests),
				slog.Duration("elapsed", now().Sub(analysisStartedAt)),
				slog.String("err", outcome.err.Error()),
			)
			partial.Models = models.names
			partial.Observed = collector.observed
			partial.Anchored = collector.anchored
			return partial, outcome.err
		}
		for _, result := range outcome.results {
			if !result.CoverageComplete {
				coverageComplete = false
			}
			collector.collect(result.Findings)
		}
	}
	logger.InfoContext(
		ctx,
		"review model analysis completed",
		slog.Int("chunks", len(chunks)),
		slog.Int("model_requests", requests),
		slog.Int("reported_findings", collector.reported),
		slog.Duration("elapsed", now().Sub(analysisStartedAt)),
	)

	sortFindings(collector.observed)
	sortFindings(collector.anchored)

	return Analysis{
		CoverageComplete: coverageComplete,
		Observed:         collector.observed,
		Anchored:         collector.anchored,
		Decision:         DecisionFor(collector.anchored, minimumImportance),
		FilesReviewed:    len(input.Files),
		Chunks:           len(chunks),
		Models:           models.names,
	}, nil
}

// findingCollector deduplicates model findings and keeps the anchored ones.
type findingCollector struct {
	fileIndex         map[string]diff.FileContext
	minimumImportance int
	seen              map[string]struct{}
	observed          []domain.Finding
	anchored          []domain.Finding
	reported          int
}

func newFindingCollector(files []diff.FileContext, minimumImportance int) *findingCollector {
	return &findingCollector{
		fileIndex:         buildFileIndex(files),
		minimumImportance: minimumImportance,
		seen:              make(map[string]struct{}),
		observed:          make([]domain.Finding, 0),
		anchored:          make([]domain.Finding, 0),
		reported:          0,
	}
}

func (collector *findingCollector) collect(findings []domain.Finding) {
	collector.reported += len(findings)
	for _, finding := range findings {
		sanitized := sanitizeFinding(finding)
		normalizedPath, pathErr := marker.NormalizePath(sanitized.Path)
		if pathErr == nil {
			sanitized.Path = normalizedPath
		}

		key := normalizedFindingKey(sanitized)
		if _, exists := collector.seen[key]; exists {
			continue
		}
		collector.seen[key] = struct{}{}

		if !isAnchored(sanitized, collector.fileIndex) {
			continue
		}
		collector.observed = append(collector.observed, sanitized)
		if sanitized.Importance >= collector.minimumImportance {
			collector.anchored = append(collector.anchored, sanitized)
		}
	}
}

// truncatedError is any model failure that stopped mid answer at the completion
// token budget.
type truncatedError interface {
	Truncated() bool
}

func truncated(err error) bool {
	var target truncatedError
	if !errors.As(err, &target) {
		return false
	}
	return target.Truncated()
}

// reviewChunk reviews one chunk and returns every result it produced.
//
// A model that reaches its completion token budget stops mid answer. Reasoning
// and answer tokens share that budget, so a chunk yielding many findings can
// exhaust it. When that happens the chunk is split in half and each half is
// reviewed instead, which asks for fewer findings per request. A chunk holding
// one hunk cannot split, so it is skipped and the review reports incomplete
// coverage rather than failing outright.
//
// Every chunk records how long its model call took, which model answered, and
// how many findings came back. Without that, a review that spends its whole
// budget reports only the chunk it died on, and nobody can tell whether the
// diff was too large or one call hung.
func reviewChunk(
	ctx context.Context,
	model Model,
	chunk diff.Chunk,
	minimumImportance int,
	models *modelSet,
	requests *int,
	now func() time.Time,
) ([]domain.ReviewResult, error) {
	logger := gklog.L(ctx)
	*requests++
	startedAt := now()
	completion, err := model.Review(ctx, buildPrompt(chunk, minimumImportance))
	elapsed := now().Sub(startedAt)
	if err == nil {
		if validateErr := completion.Result.Validate(); validateErr != nil {
			logger.ErrorContext(ctx, "validate review result", slog.String("err", validateErr.Error()))
			return nil, fmt.Errorf("validate review result: %w", validateErr)
		}
		models.add(completion.Model)
		logger.InfoContext(
			ctx,
			"review chunk completed",
			slog.Int("chunk", chunk.Index),
			slog.Int("chunks", chunk.Total),
			slog.Duration("elapsed", elapsed),
			slog.String("model", completion.Model),
			slog.Int("findings", len(completion.Result.Findings)),
			slog.Int("paths", len(chunk.Paths)),
			slog.Int("prompt_bytes", len(chunk.Text)),
		)
		return []domain.ReviewResult{completion.Result}, nil
	}
	// Every request that failed is timed here, before the truncation branch
	// decides what to do about it. A truncated request still spent its duration
	// on the review budget, and the split and skip logs below carry neither the
	// duration nor the size that caused the split.
	logger.LogAttrs(
		ctx,
		requestFailureLevel(err),
		"review chunk request failed",
		slog.Int("chunk", chunk.Index),
		slog.Int("chunks", chunk.Total),
		slog.Duration("elapsed", elapsed),
		slog.Bool("truncated", truncated(err)),
		slog.Any("paths", chunk.Paths),
		slog.Int("prompt_bytes", len(chunk.Text)),
		slog.String("err", err.Error()),
	)
	if !truncated(err) {
		return nil, fmt.Errorf("review chunk %d/%d: %w", chunk.Index, chunk.Total, err)
	}

	first, second, canSplit := chunk.Split()
	if !canSplit {
		logger.WarnContext(
			ctx,
			"review chunk skipped after truncation",
			slog.Int("chunk", chunk.Index),
			slog.Any("paths", chunk.Paths),
			slog.String("err", err.Error()),
		)
		return []domain.ReviewResult{{CoverageComplete: false, Findings: nil}}, nil
	}

	logger.InfoContext(
		ctx,
		"review chunk split after truncation",
		slog.Int("chunk", chunk.Index),
		slog.Int("hunks", len(chunk.Pieces)),
	)
	results := make([]domain.ReviewResult, 0, 2)
	for _, half := range []diff.Chunk{first, second} {
		halfResults, halfErr := reviewChunk(ctx, model, half, minimumImportance, models, requests, now)
		if halfErr != nil {
			return nil, halfErr
		}
		results = append(results, halfResults...)
	}
	return results, nil
}

// requestFailureLevel rates one failed model request. Truncation is recoverable
// because the chunk splits and retries, so it warns; any other failure ends the
// review and reports as an error.
func requestFailureLevel(err error) slog.Level {
	if truncated(err) {
		return slog.LevelWarn
	}
	return slog.LevelError
}

// modelSet records every distinct model that answered, in first use order. A
// review that starts on the primary provider and finishes on the fallback
// therefore reports both.
type modelSet struct {
	names []string
	seen  map[string]struct{}
}

func (set *modelSet) add(name string) {
	if name == "" {
		return
	}
	if set.seen == nil {
		set.seen = make(map[string]struct{})
	}
	if _, exists := set.seen[name]; exists {
		return
	}
	set.seen[name] = struct{}{}
	set.names = append(set.names, name)
}

func buildPrompt(chunk diff.Chunk, minimumImportance int) string {
	var builder strings.Builder
	builder.WriteString("Review changed lines. Return every concrete defect and assign importance from 1 through 10. ")
	builder.WriteString("Reserve 9 and 10 for defects that plausibly enable a security compromise, irreversible data loss or corruption, or a broad production outage. ")
	builder.WriteString("Rate bounded crashes, incorrect responses, maintainability problems, performance costs, and localized failures 8 or lower unless the diff proves severe impact. ")
	builder.WriteString("Put every code reference in backticks. Return suggestion as the exact replacement for the anchored changed line range only when it is complete and safe; otherwise return an empty string. ")
	builder.WriteString("The service publishes only findings with importance ")
	fmt.Fprintf(&builder, "%d", minimumImportance)
	builder.WriteString(" or higher. Do not omit a real defect because it is below that publication threshold. Review chunk ")
	fmt.Fprintf(&builder, "%d/%d", chunk.Index, chunk.Total)
	builder.WriteString(".\n")
	builder.WriteString(WrapUntrusted(chunk.Text))
	return builder.String()
}

func buildFileIndex(files []diff.FileContext) map[string]diff.FileContext {
	index := make(map[string]diff.FileContext, len(files))
	for _, file := range files {
		normalizedPath, err := marker.NormalizePath(file.Path)
		if err != nil {
			continue
		}
		index[normalizedPath] = file
	}
	return index
}

func inputCoverageComplete(files []diff.FileContext) bool {
	for _, file := range files {
		if !file.CoverageComplete {
			return false
		}
	}
	return true
}

func isAnchored(finding domain.Finding, fileIndex map[string]diff.FileContext) bool {
	normalizedPath, err := marker.NormalizePath(finding.Path)
	if err != nil {
		return false
	}
	file, ok := fileIndex[normalizedPath]
	if !ok {
		return false
	}
	if !diff.ValidRange(file.ChangedRightLines, file.ChangedRightHunks, finding.StartLine, finding.EndLine) {
		return false
	}
	return true
}

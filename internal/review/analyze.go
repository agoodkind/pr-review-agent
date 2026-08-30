package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

// chunkFailure records one chunk this run could not read.
type chunkFailure struct {
	chunk int
	err   error
}

// chunkFailureReasons names every chunk that failed and why, quoting the cause
// verbatim. Only the service log carries these: a provider sentence is text
// this service does not control, and every published surface is permanent.
func chunkFailureReasons(failures []chunkFailure) []string {
	reasons := make([]string, 0, len(failures))
	for _, failure := range failures {
		reasons = append(reasons, fmt.Sprintf("chunk %d: %s", failure.chunk, failure.err.Error()))
	}
	return reasons
}

// unreadChunkNumbers names which chunks went unread, which is diagnosis a
// reader can act on with no provider text reaching a public surface.
func unreadChunkNumbers(failures []chunkFailure) string {
	numbers := make([]string, 0, len(failures))
	for _, failure := range failures {
		numbers = append(numbers, strconv.Itoa(failure.chunk))
	}
	return strings.Join(numbers, ", ")
}

// logChunkFailures reports every chunk that failed in one line, so a reader can
// tell how much of the diff went unread and why without opening each chunk.
func logChunkFailures(ctx context.Context, failures []chunkFailure, chunks int, requests int) {
	if len(failures) == 0 {
		return
	}
	gklog.L(ctx).ErrorContext(
		ctx,
		"review chunks unread",
		slog.Int("chunks", chunks),
		slog.Int("chunks_failed", len(failures)),
		slog.Int("model_requests", requests),
		slog.Any("causes", chunkFailureReasons(failures)),
	)
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
		sanitized := normalizeFinding(finding)

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

// normalizeFinding puts one model finding into the shape the rest of the review
// works with: sanitized text and a path that matches the diff.
func normalizeFinding(finding domain.Finding) domain.Finding {
	sanitized := sanitizeFinding(finding)
	if normalizedPath, err := marker.NormalizePath(sanitized.Path); err == nil {
		sanitized.Path = normalizedPath
	}
	return sanitized
}

// groundedFindings returns the findings whose evidence appears in the source
// the model was shown, normalized and ready for the collector.
//
// It runs before the collector rather than only before publication. A finding
// aimed at another chunk used to reach the collector, count toward the run's own
// analysis, and carry weight in what the review concluded, while the publication
// test refused it and no reader ever saw it. A claim nobody can be shown must
// not decide anything, so it is refused once, at the door.
func groundedFindings(
	ctx context.Context,
	findings []domain.Finding,
	fileIndex map[string]diff.FileContext,
	chunkText string,
) []domain.Finding {
	logger := gklog.L(ctx)
	grounded := make([]domain.Finding, 0, len(findings))
	for _, finding := range findings {
		sanitized := normalizeFinding(finding)
		if !findingGrounded(sanitized, chunkText, fileIndex) {
			logger.WarnContext(
				ctx,
				"finding discarded, evidence not in the source shown",
				slog.String("path", sanitized.Path),
				slog.String("title", sanitized.Title),
				slog.String("evidence", sanitized.Evidence),
			)
			continue
		}
		grounded = append(grounded, sanitized)
	}
	return grounded
}

// eligibleFindings returns the grounded findings that anchor to changed lines
// and meet the importance floor.
//
// Everything it returns is posted, because the review stands behind every defect
// it reports and rationing them is how a reader ends up acting on the wrong one.
// Duplicates stay in, because the caller suppresses them against what the pull
// request already carries.
func eligibleFindings(
	findings []domain.Finding,
	fileIndex map[string]diff.FileContext,
	minimumImportance int,
) []domain.Finding {
	eligible := make([]domain.Finding, 0, len(findings))
	for _, finding := range findings {
		sanitized := normalizeFinding(finding)
		if !isAnchored(sanitized, fileIndex) {
			continue
		}
		if sanitized.Importance < minimumImportance {
			continue
		}
		eligible = append(eligible, sanitized)
	}
	return eligible
}

// findingGrounded reports whether the finding's evidence is a whole line of the
// source the model was shown: the chunk text it reviewed, or the current content
// of the file the finding anchors to. A finding without evidence is ungrounded,
// which is also how an answer from an older schema reads, so a claim quoting
// code the model never saw cannot pass.
func findingGrounded(
	finding domain.Finding,
	chunkText string,
	fileIndex map[string]diff.FileContext,
) bool {
	evidence := strings.TrimSpace(finding.Evidence)
	if evidence == "" {
		return false
	}
	if matchesDiffLine(chunkText, evidence) {
		return true
	}
	file, ok := fileIndex[finding.Path]
	if !ok {
		return false
	}
	return matchesFileLine(file.CurrentContent, evidence)
}

// matchesDiffLine reports whether evidence is one whole line of the diff the
// model was shown, ignoring the marker a unified diff puts at the start of every
// line.
//
// Substring containment accepted any fragment that happened to appear somewhere
// in the text, a file header or a lone brace included, so the evidence gate
// admitted claims the model had not actually read a line for. A line copied
// verbatim, which is what the prompt asks for, always equals one whole line.
func matchesDiffLine(chunkText string, evidence string) bool {
	for line := range strings.SplitSeq(chunkText, "\n") {
		if strings.TrimSpace(stripDiffMarker(line)) == evidence {
			return true
		}
	}
	return false
}

// matchesFileLine reports whether evidence is one whole line of file content.
func matchesFileLine(content string, evidence string) bool {
	for line := range strings.SplitSeq(content, "\n") {
		if strings.TrimSpace(line) == evidence {
			return true
		}
	}
	return false
}

// stripDiffMarker removes the single leading character a unified diff body line
// carries. Exactly one is removed, so a diff header such as "+++ b/main.go"
// cannot be mistaken for the source line "+ b/main.go".
func stripDiffMarker(line string) string {
	if line == "" {
		return line
	}
	switch line[0] {
	case '+', '-', ' ':
		return line[1:]
	default:
		return line
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
// how many findings came back. Without that, a run that leaves chunks unread
// names only the chunk it stopped on, and nobody can tell a slow provider from
// one hung call.
func reviewChunk(
	ctx context.Context,
	model Model,
	chunk diff.Chunk,
	minimumImportance int,
	disputes string,
	models *modelSet,
	requests *int,
	now func() time.Time,
) ([]domain.ReviewResult, error) {
	logger := gklog.L(ctx)
	*requests++
	startedAt := now()
	completion, err := model.Review(ctx, buildPrompt(chunk, minimumImportance, disputes))
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
		halfResults, halfErr := reviewChunk(ctx, model, half, minimumImportance, disputes, models, requests, now)
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

// buildPrompt assembles one chunk's model prompt.
//
// disputes is the open thread context, empty when the pull request carries no
// open finding of the service's own. It comes first, because what has already
// been raised and answered has to be in view before the model reads the code
// and decides what to say about it.
func buildPrompt(chunk diff.Chunk, minimumImportance int, disputes string) string {
	var builder strings.Builder
	builder.WriteString(disputes)
	builder.WriteString("Review changed lines. Return every concrete defect and assign importance from 1 through 10. ")
	builder.WriteString("Reserve 9 and 10 for defects that plausibly enable a security compromise, irreversible data loss or corruption, or a broad production outage. ")
	builder.WriteString("Rate bounded crashes, incorrect responses, maintainability problems, performance costs, and localized failures 8 or lower unless the diff proves severe impact. ")
	builder.WriteString("Put every code reference in backticks. Return suggestion as the exact replacement for the anchored changed line range only when it is complete and safe; otherwise return an empty string. ")
	builder.WriteString("Copy into evidence one line from the supplied source, verbatim and unmodified, that the finding relies on. A finding whose evidence does not appear in the supplied source is discarded. ")
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

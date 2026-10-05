package review

import (
	"context"
	"encoding/json"
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
	"goodkind.io/pr-review-agent/internal/reviewrules"
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
	metadataIndex     map[domain.FindingTarget]diff.MetadataSource
	minimumImportance int
	seen              map[string]struct{}
	observed          []domain.Finding
	anchored          []domain.Finding
	reported          int
}

func newFindingCollector(files []diff.FileContext, metadata []diff.MetadataSource, minimumImportance int) *findingCollector {
	return &findingCollector{
		fileIndex:         BuildFileIndex(files),
		metadataIndex:     BuildMetadataIndex(metadata),
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

		if !isAnchored(sanitized, collector.fileIndex, collector.metadataIndex) {
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
	sanitized.Surface = finding.EffectiveSurface()
	if sanitized.Surface == domain.FindingFile {
		if normalizedPath, err := marker.NormalizePath(sanitized.Path); err == nil {
			sanitized.Path = normalizedPath
		}
	}
	return sanitized
}

// GroundedFindings returns the findings whose evidence appears in the source
// the model was shown, normalized and ready for the collector.
//
// It runs before the collector rather than only before publication. A finding
// aimed at another chunk used to reach the collector, count toward the run's own
// analysis, and carry weight in what the review concluded, while the publication
// test refused it and no reader ever saw it. A claim nobody can be shown must
// not decide anything, so it is refused once, at the door.
func GroundedFindings(
	ctx context.Context,
	findings []domain.Finding,
	fileIndex map[string]diff.FileContext,
	metadataIndex map[domain.FindingTarget]diff.MetadataSource,
	chunkText string,
) []domain.Finding {
	logger := gklog.L(ctx)
	grounded := make([]domain.Finding, 0, len(findings))
	for _, finding := range findings {
		sanitized := normalizeFinding(finding)
		if !findingGrounded(sanitized, chunkText, fileIndex, metadataIndex) {
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

// EligibleFindings returns the grounded findings that anchor to changed lines
// and meet the importance floor.
//
// Everything it returns is posted, because the review stands behind every defect
// it reports and rationing them is how a reader ends up acting on the wrong one.
// Duplicates stay in, because the caller suppresses them against what the pull
// request already carries.
func EligibleFindings(
	findings []domain.Finding,
	fileIndex map[string]diff.FileContext,
	metadataIndex map[domain.FindingTarget]diff.MetadataSource,
	minimumImportance int,
) []domain.Finding {
	eligible := make([]domain.Finding, 0, len(findings))
	for _, finding := range findings {
		sanitized := normalizeFinding(finding)
		if !isAnchored(sanitized, fileIndex, metadataIndex) {
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
	metadataIndex map[domain.FindingTarget]diff.MetadataSource,
) bool {
	if finding.EffectiveSurface() != domain.FindingFile {
		source, found := metadataIndex[finding.Target()]
		return found && metadataFindingAnchored(finding, source) && matchesFileLine(chunkText, finding.Evidence)
	}
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
// model was shown.
//
// The diff marks every line, and the prompt asks for a verbatim copy, so an
// honest answer about an added line arrives either as "+return err" or as
// "return err" depending on whether the model kept the marker. Both ground.
//
// Only the source side is stripped. Stripping the evidence too made "-return
// err" match an added "+return err", which are different lines saying opposite
// things about the change, and let a finding about deleted code stand on code
// that is still there.
func matchesDiffLine(chunkText string, evidence string) bool {
	want := strings.TrimSpace(evidence)
	if want == "" {
		return false
	}
	for line := range strings.SplitSeq(chunkText, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
		if strings.TrimSpace(stripDiffMarker(line)) == want {
			return true
		}
	}
	return false
}

// matchesFileLine reports whether evidence is one whole line of file content.
//
// Nothing is stripped here, because file content carries no diff markers. A
// leading "-" in a file is part of the line, so stripping it let the evidence
// "item" ground against the real source line "- item", which is a different
// line the model never quoted.
func matchesFileLine(content string, evidence string) bool {
	want := strings.TrimSpace(evidence)
	if want == "" {
		return false
	}
	for line := range strings.SplitSeq(content, "\n") {
		if strings.TrimSpace(line) == want {
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

// chunkAnalysis is what one chunk produced: the model's answers, and what this
// service observed about the calls that produced them.
//
// The second half belongs to the service rather than the model. A hunk nobody
// could get a whole answer about is a hunk nobody read, and only the caller of
// the model is in a position to know that happened.
type chunkAnalysis struct {
	Results []domain.ReviewResult
	// Unreadable names hunks whose answer never arrived whole, which is a
	// shortfall a later run reaches the same way.
	Unreadable []unreadHunk
}

// merge folds one recursive half's outcome into this one.
func (analysis *chunkAnalysis) merge(other chunkAnalysis) {
	analysis.Results = append(analysis.Results, other.Results...)
	analysis.Unreadable = append(analysis.Unreadable, other.Unreadable...)
}

// unreadableHunksIn names the hunks of a chunk nobody could get an answer about.
//
// A chunk reaches this only when it could not split, so it holds one hunk at
// most. Naming that hunk by path and coordinates is what lets the run report
// what went unread instead of a bare count with nothing to point at.
func unreadableHunksIn(chunk diff.Chunk) []unreadHunk {
	hunks := make([]unreadHunk, 0, len(chunk.Pieces))
	for _, piece := range chunk.Pieces {
		hunks = append(hunks, unreadPiece(piece, truncatedAnswerReason))
	}
	return hunks
}

// reviewChunk reviews one chunk and returns every result it produced.
//
// A model that reaches its completion token budget stops mid answer. Reasoning
// and answer tokens share that budget, so a chunk yielding many findings can
// exhaust it. When that happens the chunk is split in half and each half is
// reviewed instead, which asks for fewer findings per request. A chunk holding
// one hunk cannot split, so the run names that hunk as one nobody read and
// carries on rather than failing outright.
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
	policy reviewrules.Policy,
	maximumBytes int,
	files []diff.FileContext,
	disputes string,
	models *modelSet,
	requests *int,
	now func() time.Time,
) (chunkAnalysis, error) {
	logger := gklog.L(ctx)
	nothing := chunkAnalysis{Results: nil, Unreadable: nil}
	bounded, err := BoundReviewChunks([]diff.Chunk{chunk}, files, minimumImportance, disputes, policy, maximumBytes)
	if err != nil {
		logger.ErrorContext(ctx, "Review input size preparation failed", "err", err)
		return nothing, err
	}
	if len(bounded) > 1 {
		return reviewChunkParts(ctx, model, bounded, minimumImportance, policy, maximumBytes, files, disputes, models, requests, now)
	}
	chunk = bounded[0]
	prompt, err := BuildPrompt(chunk, minimumImportance, disputes, policy, maximumBytes)
	if err != nil {
		logger.ErrorContext(ctx, "Review prompt rendering failed", "err", err)
		return nothing, err
	}
	*requests++
	startedAt := now()
	completion, err := model.Review(ctx, prompt)
	elapsed := now().Sub(startedAt)
	if err == nil {
		if validateErr := completion.Result.Validate(); validateErr != nil {
			logger.ErrorContext(ctx, "validate review result", slog.String("err", validateErr.Error()))
			return nothing, fmt.Errorf("validate review result: %w", validateErr)
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
			slog.Int("prompt_bytes", len(prompt)),
		)
		return chunkAnalysis{
			Results:    []domain.ReviewResult{completion.Result},
			Unreadable: nil,
		}, nil
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
		slog.Int("prompt_bytes", len(prompt)),
		slog.String("err", err.Error()),
	)
	if !truncated(err) {
		return nothing, fmt.Errorf("review chunk %d/%d: %w", chunk.Index, chunk.Total, err)
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
		return chunkAnalysis{Results: nil, Unreadable: unreadableHunksIn(chunk)}, nil
	}

	logger.InfoContext(
		ctx,
		"review chunk split after truncation",
		slog.Int("chunk", chunk.Index),
		slog.Int("hunks", len(chunk.Pieces)),
	)
	return reviewChunkParts(ctx, model, []diff.Chunk{first, second}, minimumImportance, policy, maximumBytes, files, disputes, models, requests, now)
}

func reviewChunkParts(ctx context.Context, model Model, chunks []diff.Chunk, minimumImportance int, policy reviewrules.Policy, maximumBytes int, files []diff.FileContext, disputes string, models *modelSet, requests *int, now func() time.Time) (chunkAnalysis, error) {
	combined := chunkAnalysis{Results: nil, Unreadable: nil}
	for _, chunk := range chunks {
		part, err := reviewChunk(ctx, model, chunk, minimumImportance, policy, maximumBytes, files, disputes, models, requests, now)
		if err != nil {
			return chunkAnalysis{Results: nil, Unreadable: nil}, err
		}
		combined.merge(part)
	}
	return combined, nil
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

// BuildPrompt assembles one chunk's model prompt for production and live evaluation.
//
// disputes is the relevant reviewer context, empty when the pull request
// carries no open finding or resolved finding from this head. It comes first,
// because what has already been raised and answered has to be in view before
// the model reads the code and decides what to say about it.
func BuildPrompt(chunk diff.Chunk, minimumImportance int, disputes string, policy reviewrules.Policy, maximumBytes int) (string, error) {
	prompt, err := renderChunkPrompt(chunk, minimumImportance, disputes, policy)
	if err != nil {
		return "", err
	}
	if len(prompt) > maximumBytes {
		return "", &diff.PromptSizeError{Actual: len(prompt), Limit: maximumBytes}
	}
	return prompt, nil
}

// BoundReviewChunks measures the configured input template and supplied context.
func BoundReviewChunks(chunks []diff.Chunk, files []diff.FileContext, minimumImportance int, disputes string, policy reviewrules.Policy, maximumBytes int) ([]diff.Chunk, error) {
	bounded, err := diff.BoundChunks(chunks, files, maximumBytes, func(chunk diff.Chunk) (int, error) {
		prompt, err := renderChunkPrompt(chunk, minimumImportance, disputes, policy)
		return len(prompt), err
	})
	if err != nil {
		slog.Warn("Review input budget preparation failed", "err", err)
		return nil, fmt.Errorf("prepare bounded review input: %w", err)
	}
	return bounded, nil
}

func renderChunkPrompt(chunk diff.Chunk, minimumImportance int, disputes string, policy reviewrules.Policy) (string, error) {
	rows, err := diff.ChangedSourceRows(chunk)
	if err != nil {
		slog.Warn("Changed source row preparation failed", "err", err)
		return "", fmt.Errorf("prepare changed source rows: %w", err)
	}
	anchors, err := json.Marshal(rows)
	if err != nil {
		slog.Warn("Changed source row encoding failed", "err", err)
		return "", fmt.Errorf("encode changed source rows: %w", err)
	}
	var data reviewrules.PromptData
	data.MinimumImportance = minimumImportance
	data.Input = policy.WrapUntrusted(chunk.Text)
	data.Anchors = policy.WrapUntrusted(string(anchors))
	data.Disputes = disputes
	data.Index = chunk.Index
	data.Total = chunk.Total
	return renderPrompt(policy, "review.input", data)
}

// BuildFileIndex applies the path normalization used by publication.
func BuildFileIndex(files []diff.FileContext) map[string]diff.FileContext {
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

func isAnchored(finding domain.Finding, fileIndex map[string]diff.FileContext, metadataIndex map[domain.FindingTarget]diff.MetadataSource) bool {
	if finding.EffectiveSurface() != domain.FindingFile {
		source, found := metadataIndex[finding.Target()]
		return found && metadataFindingAnchored(finding, source)
	}
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

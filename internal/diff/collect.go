package diff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// CoverageGap names why one file could not be collected whole.
//
// The distinction it carries is whether a later run would fare any better. A
// binary file, a file GitHub supplies no patch for, and a patch that cannot be
// read whole all come back the same way on every later run, so a caller that
// treats them as temporary promises a next push that cannot deliver. Content
// that failed to load is the opposite: the call went wrong once, and the next
// run very likely succeeds.
type CoverageGap string

// The gaps one file can carry. CoverageGapNone means the file was collected
// whole, which is the only value that leaves CoverageComplete true.
const (
	CoverageGapNone            CoverageGap = ""
	CoverageGapBinary          CoverageGap = "binary"
	CoverageGapPatchAbsent     CoverageGap = "patch_absent"
	CoverageGapPatchUnreadable CoverageGap = "patch_unreadable"
	// CoverageGapContentMissing is a file GitHub answered about and refused: it
	// is not there at this commit, or it may not be served. Every later run asks
	// the same question and gets the same answer.
	CoverageGapContentMissing CoverageGap = "content_missing"
	// CoverageGapContentUnavailable is a content load that did not get an
	// answer. A rate limit, an outage, or a timeout produces it, and the next run
	// very likely succeeds. An error this service cannot classify lands here
	// too, because calling an unknown cause permanent would tell a person to
	// split a pull request over something that may fix itself in a minute.
	CoverageGapContentUnavailable CoverageGap = "content_unavailable"
)

// Recurs reports whether this gap would come back identically on a later run.
func (gap CoverageGap) Recurs() bool {
	switch gap {
	case CoverageGapBinary, CoverageGapPatchAbsent, CoverageGapPatchUnreadable, CoverageGapContentMissing:
		return true
	case CoverageGapNone, CoverageGapContentUnavailable:
		return false
	default:
		return false
	}
}

// contentGapFor classifies why one file's content did not load.
//
// GitHub answering that the file is not there, is gone, or may not be served is
// a permanent answer: the same request returns the same thing on every later
// run, so a run that keeps calling it temporary reports incomplete coverage
// forever while never naming anything a person could act on. Everything else,
// including an error with no status this service recognizes, is treated as a
// call that did not get through.
func contentGapFor(err error) CoverageGap {
	var apiErr githubapp.APIError
	if !errors.As(err, &apiErr) {
		return CoverageGapContentUnavailable
	}
	switch apiErr.StatusCode {
	case http.StatusNotFound, http.StatusGone, http.StatusUnavailableForLegalReasons:
		return CoverageGapContentMissing
	default:
		return CoverageGapContentUnavailable
	}
}

// FileContext is one changed file with patch metadata and current content.
type FileContext struct {
	Path              string
	Status            string
	Patch             string
	CurrentContent    string
	Attributes        map[string]string
	ChangedRightLines map[int]struct{}
	ChangedRightHunks map[int]int
	CoverageComplete  bool
	// Gap names why CoverageComplete is false, and is CoverageGapNone whenever
	// it is true. Collection sets the two together, so a caller reading one
	// never disagrees with the other.
	Gap            CoverageGap
	sourceMetadata string
}

// IsSubmodule reports whether repository metadata identifies a pinned submodule.
func (file FileContext) IsSubmodule() bool {
	return strings.HasPrefix(file.sourceMetadata, "Submodule repository: ")
}

// markGap records why this file could not be collected whole. It is the only
// place the two coverage fields are written, so they cannot drift apart.
//
// A gap that recurs is never replaced by one that does not. A file whose patch
// cannot be read whole also tends to fail the content load that follows, and
// letting the second overwrite the first would report a permanent gap as a
// temporary one.
func (file *FileContext) markGap(gap CoverageGap) {
	file.CoverageComplete = false
	if file.Gap.Recurs() {
		return
	}
	file.Gap = gap
}

func (file *FileContext) collectChangedLines() bool {
	changedLines, changedHunks, err := ChangedRightLines(file.Patch)
	if err != nil {
		file.markGap(CoverageGapPatchUnreadable)
		return false
	}
	file.ChangedRightLines = changedLines
	file.ChangedRightHunks = changedHunks
	return true
}

// ReviewInput is the collected pull request context for one review pass.
type ReviewInput struct {
	PullRequest githubapp.PullRequest
	Files       []FileContext
	Metadata    []MetadataSource
	// MergeBase is the commit the patches are measured from, which is not always
	// the commit the range was asked for: GitHub compares from where two commits
	// last agreed. It is empty when the whole pull request was listed rather than
	// a range compared. Anything mapping coordinates from the requested base has
	// to check this first, or it maps from a commit that was never in the patch.
	MergeBase domain.HeadSHA
}

// Piece contains one complete diff fragment and its source context.
type Piece struct {
	Path     string
	Text     string
	Patch    string
	Metadata *MetadataSource
	// Header is the hunk's "@@ -a,b +c,d @@" coordinates, without the trailing
	// source line git appends to them. It is what names this piece to a reader
	// when nobody could read it.
	Header           string
	CoverageComplete bool
	// Oversized marks an indivisible fragment that exceeds one request.
	Oversized   bool
	fileContext *FileContext
}

// Chunk is one model input slice with complete hunks from one or more files.
// It keeps the pieces it was built from, so a caller that cannot process the
// whole chunk can split it without re-deriving anything from the diff.
type Chunk struct {
	RelationshipReview bool
	Index              int
	Total              int
	Text               string
	Pieces             []Piece
	Paths              []string
	Metadata           []MetadataSource
	CoverageComplete   bool
}

// Split divides between pieces or complete source rows within one file piece.
func (chunk Chunk) Split() (Chunk, Chunk, bool) {
	if chunk.RelationshipReview {
		return emptyChunk(), emptyChunk(), false
	}
	if len(chunk.Pieces) == 1 {
		first, second, ok := splitFilePiece(chunk.Pieces[0])
		if ok {
			return chunkFromPieces([]Piece{first}, chunk.Index, chunk.Total), chunkFromPieces([]Piece{second}, chunk.Index, chunk.Total), true
		}
	}
	if len(chunk.Pieces) < 2 {
		return emptyChunk(), emptyChunk(), false
	}
	middle := len(chunk.Pieces) / 2
	first := chunkFromPieces(chunk.Pieces[:middle], chunk.Index, chunk.Total)
	second := chunkFromPieces(chunk.Pieces[middle:], chunk.Index, chunk.Total)
	return first, second, true
}

func emptyChunk() Chunk {
	return Chunk{
		RelationshipReview: false,
		Index:              0,
		Total:              0,
		Text:               "",
		Pieces:             nil,
		Paths:              nil,
		Metadata:           nil,
		CoverageComplete:   true,
	}
}

func chunkFromPieces(pieces []Piece, index int, total int) Chunk {
	builder := newChunkBuilder()
	for _, piece := range pieces {
		if builder.hasText() {
			builder.appendSeparator()
		}
		builder.appendText(piece.Text)
		builder.addPiece(piece)
		builder.markIncomplete(!piece.CoverageComplete)
		builder.addPath(piece.Path)
	}
	chunk, _ := builder.build()
	chunk.Index = index
	chunk.Total = total
	return chunk
}

// Source loads changed files and repository content for diff collection.
type Source interface {
	ListChangedFiles(context.Context, int64, domain.Repository, int) ([]githubapp.ChangedFile, error)
	ListPullRequestCommits(context.Context, int64, domain.Repository, int) ([]githubapp.PullRequestCommit, error)
	GetFile(context.Context, int64, domain.Repository, string, domain.HeadSHA) ([]byte, error)
	Compare(context.Context, int64, domain.Repository, domain.HeadSHA, domain.HeadSHA) (githubapp.Comparison, error)
}

// Collector gathers pull request patches and file content for review.
type Collector struct {
	source Source
}

// NewCollector constructs a diff collector backed by one source.
func NewCollector(source Source) *Collector {
	return &Collector{source: source}
}

// Collect loads changed files, patches, and current content for one pull request.
func (collector *Collector) Collect(
	ctx context.Context,
	ref domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
) (ReviewInput, error) {
	changedFiles, err := collector.source.ListChangedFiles(
		ctx,
		ref.InstallationID,
		ref.Repository,
		ref.Number,
	)
	if err != nil {
		slog.ErrorContext(ctx, "list changed files", slog.String("err", err.Error()))
		return ReviewInput{}, fmt.Errorf("list changed files: %w", err)
	}

	files, mergeBase, err := collector.collectFiles(ctx, ref, pullRequest, changedFiles, "")
	if err != nil {
		return ReviewInput{}, err
	}
	metadata, err := collector.collectMetadata(ctx, ref, pullRequest)
	if err != nil {
		return ReviewInput{}, err
	}
	return ReviewInput{
		PullRequest: pullRequest,
		Files:       files,
		Metadata:    metadata,
		MergeBase:   mergeBase,
	}, nil
}

// CollectRange loads changed files, patches, and current content scoped to the
// range since a previously reviewed commit. An empty base means no prior
// review exists, so it collects the same full file list Collect does; a
// review never repeats a commit range once the range since the last review is
// the unit of work.
func (collector *Collector) CollectRange(
	ctx context.Context,
	ref domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	base domain.HeadSHA,
) (ReviewInput, error) {
	if base == "" {
		return collector.Collect(ctx, ref, pullRequest)
	}

	changedFiles, err := collector.source.Compare(
		ctx,
		ref.InstallationID,
		ref.Repository,
		base,
		pullRequest.Head,
	)
	if err != nil {
		// A force push or a history rewrite can leave the recorded base
		// unreachable, and GitHub then refuses the comparison outright. Failing
		// here would strand the pull request: every later run reads the same
		// dead base from the marker and aborts the same way, so the review
		// never recovers on its own. Reviewing the whole pull request instead
		// costs one larger run and leaves nothing unread.
		if baseUnreachable(err) {
			slog.WarnContext(
				ctx,
				"recorded base is no longer comparable, reviewing the whole pull request",
				slog.String("base", string(base)),
				slog.String("err", err.Error()),
			)
			return collector.Collect(ctx, ref, pullRequest)
		}
		slog.ErrorContext(ctx, "compare commit range", slog.String("err", err.Error()))
		return ReviewInput{}, fmt.Errorf("compare %s to %s: %w", base, pullRequest.Head, err)
	}

	// A rewritten history makes GitHub compare from an older merge base. That
	// range can include files outside the pull request, whose lines GitHub will
	// refuse as review comment anchors. Review the current pull request instead.
	if changedFiles.MergeBase != "" && changedFiles.MergeBase != base {
		slog.WarnContext(
			ctx,
			"review history diverged, reviewing the whole pull request",
			slog.String("requested", string(base)),
			slog.String("merge_base", string(changedFiles.MergeBase)),
		)
		return collector.Collect(ctx, ref, pullRequest)
	}
	files, mergeBase, err := collector.collectFiles(ctx, ref, pullRequest, changedFiles.Files, changedFiles.MergeBase)
	if err != nil {
		return ReviewInput{}, err
	}
	metadata, err := collector.collectMetadata(ctx, ref, pullRequest)
	if err != nil {
		return ReviewInput{}, err
	}
	return ReviewInput{
		PullRequest: pullRequest,
		Files:       files,
		MergeBase:   mergeBase,
		Metadata:    metadata,
	}, nil
}

func (collector *Collector) collectMetadata(ctx context.Context, ref domain.PullRequestRef, pullRequest githubapp.PullRequest) ([]MetadataSource, error) {
	var commits []githubapp.PullRequestCommit
	if pullRequest.CommitCount > 0 {
		var err error
		commits, err = collector.source.ListPullRequestCommits(ctx, ref.InstallationID, ref.Repository, ref.Number)
		if err != nil {
			slog.WarnContext(ctx, "Load commit prose failed", "err", err)
			return nil, fmt.Errorf("load commit prose: %w", err)
		}
		if len(commits) != pullRequest.CommitCount {
			return nil, fmt.Errorf("GitHub returned %d of %d pull request commit messages", len(commits), pullRequest.CommitCount)
		}
	}
	return PullRequestMetadata(pullRequest, commits), nil
}

// baseUnreachable reports whether GitHub refused a comparison because the base
// commit is gone rather than because the call went wrong. A missing commit
// answers 404, and a range GitHub cannot compute answers 422. Every other
// failure, including rate limits and outages, is transient and must keep
// failing so a later run retries it rather than silently reviewing more than
// it was asked to.
func baseUnreachable(err error) bool {
	var apiErr githubapp.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusNotFound ||
		apiErr.StatusCode == http.StatusUnprocessableEntity
}

// collectFiles orders changed files deterministically and collects each one's
// patch metadata and current content, the shared work behind both Collect and
// CollectRange.
func (collector *Collector) collectFiles(
	ctx context.Context,
	ref domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	changedFiles []githubapp.ChangedFile,
	base domain.HeadSHA,
) ([]FileContext, domain.HeadSHA, error) {
	sort.Slice(changedFiles, func(left, right int) bool {
		return changedFiles[left].Path < changedFiles[right].Path
	})

	files := make([]FileContext, 0, len(changedFiles))
	recovery := newPatchRecovery(collector.source, ref, pullRequest, base)
	attributes := newRepositoryAttributes(collector.source, ref)
	for _, changedFile := range changedFiles {
		file, err := collectFile(ctx, changedFile, recovery)
		if err != nil {
			return nil, "", err
		}
		attributes.remember(file, pullRequest.Head)
		file.Attributes, err = attributes.forChange(ctx, changedFile, recovery)
		if err != nil {
			return nil, "", err
		}
		files = append(files, file)
	}
	return files, recovery.base, nil
}

func collectFile(
	ctx context.Context,
	changedFile githubapp.ChangedFile,
	recovery *patchRecovery,
) (FileContext, error) {
	fileContext := FileContext{
		Path:              changedFile.Path,
		Status:            changedFile.Status,
		Patch:             changedFile.Patch,
		CurrentContent:    "",
		ChangedRightLines: map[int]struct{}{},
		ChangedRightHunks: map[int]int{},
		CoverageComplete:  true,
		Gap:               CoverageGapNone,
		sourceMetadata:    "",
	}

	if fileLooksBinary(changedFile.Status, changedFile.Patch) {
		fileContext.markGap(CoverageGapBinary)
		return fileContext, nil
	}
	needsRecovery := patchNeedsRecovery(changedFile)
	canRecover := recovery.base != "" || recovery.pr.Base != ""
	if !changedFile.PatchPresent {
		fileContext.markGap(CoverageGapPatchAbsent)
	} else {
		if !fileContext.collectChangedLines() && !canRecover {
			return fileContext, nil
		}
		parsed, err := parsePatch(changedFile.Patch)
		if err != nil || !parsed.complete {
			fileContext.markGap(CoverageGapPatchUnreadable)
		}
	}
	if needsRecovery && fileContext.Gap == CoverageGapNone {
		fileContext.markGap(CoverageGapPatchUnreadable)
	}

	loaded, err := loadCurrentFile(ctx, recovery, &fileContext)
	if err != nil {
		return FileContext{}, err
	}
	if !loaded {
		return fileContext, nil
	}
	if needsRecovery && canRecover {
		patch, err := recovery.recover(ctx, changedFile, fileContext.CurrentContent)
		if errors.Is(err, ErrNonTextPatch) {
			fileContext.Gap = CoverageGapBinary
			fileContext.CoverageComplete = false
			return fileContext, nil
		}
		if err != nil {
			slog.ErrorContext(ctx, "recover file patch", slog.String("path", changedFile.Path), slog.String("err", err.Error()))
			return FileContext{}, fmt.Errorf("recover patch for %s: %w", changedFile.Path, err)
		}
		fileContext.Patch = patch
		fileContext.Gap = CoverageGapNone
		fileContext.CoverageComplete = true
		if !fileContext.collectChangedLines() {
			return FileContext{}, fmt.Errorf("recovered patch for %s is invalid", changedFile.Path)
		}
	}
	return fileContext, nil
}

func loadCurrentFile(ctx context.Context, recovery *patchRecovery, file *FileContext) (bool, error) {
	if file.Status == "removed" {
		return true, nil
	}
	content, err := recovery.readFile(ctx, file.Path, recovery.pr.Head)
	if err == nil {
		file.CurrentContent = string(content.Data)
		file.sourceMetadata = content.Metadata
		return true, nil
	}
	if file.Gap == CoverageGapPatchAbsent && recovery.base == "" && recovery.pr.Base == "" {
		slog.WarnContext(ctx, "load omitted file context", slog.String("path", file.Path), slog.String("err", err.Error()))
		return false, nil
	}
	gap := contentGapFor(err)
	if !gap.Recurs() {
		slog.ErrorContext(ctx, "load file content", slog.String("path", file.Path), slog.String("err", err.Error()))
		return false, fmt.Errorf("load content for %s: %w", file.Path, err)
	}
	file.markGap(gap)
	return false, nil
}

func fileLooksBinary(status string, patch string) bool {
	if status == "binary" {
		return true
	}
	for line := range strings.SplitSeq(patch, "\n") {
		if strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ") {
			return true
		}
	}
	return false
}

// ChunkInput splits review input into deterministic chunks no larger than maxSize bytes.
func ChunkInput(input ReviewInput, maxSize int) ([]Chunk, error) {
	if maxSize < 1 {
		maxSize = 1
	}

	pieces := make([]Piece, 0)
	for _, file := range input.Files {
		if fileLooksBinary(file.Status, file.Patch) {
			continue
		}
		if strings.TrimSpace(file.Patch) == "" {
			continue
		}

		parsed, err := parsePatch(file.Patch)
		if err != nil {
			continue
		}
		for _, hunk := range parsed.hunks {
			pieces = append(pieces, fileHunkPieces(&file, hunk, maxSize)...)
		}
	}

	for _, metadata := range input.Metadata {
		fragments := metadataPieces(metadata, maxSize)
		pieces = append(pieces, fragments...)
	}
	if len(pieces) == 0 {
		return nil, nil
	}

	chunks := make([]Chunk, 0)
	current := newChunkBuilder()
	currentSize := 0

	flush := func() {
		if chunk, ok := current.build(); ok {
			chunks = append(chunks, chunk)
		}
		current = newChunkBuilder()
		currentSize = 0
	}

	for _, piece := range pieces {
		if current.hasText() && currentSize+1+len(piece.Text) > maxSize {
			flush()
		}
		if current.hasText() {
			current.appendSeparator()
			currentSize++
		}
		current.appendText(piece.Text)
		currentSize += len(piece.Text)
		current.addPiece(piece)
		current.markIncomplete(!piece.CoverageComplete)
		current.addPath(piece.Path)
	}
	flush()

	for index := range chunks {
		chunks[index].Index = index + 1
		chunks[index].Total = len(chunks)
	}
	return chunks, nil
}

type chunkBuilder struct {
	text             string
	pieces           []Piece
	paths            []string
	metadata         []MetadataSource
	coverageComplete bool
	pathSeen         map[string]struct{}
}

func newChunkBuilder() *chunkBuilder {
	return &chunkBuilder{
		text:             "",
		pieces:           make([]Piece, 0),
		paths:            make([]string, 0),
		metadata:         nil,
		coverageComplete: true,
		pathSeen:         make(map[string]struct{}),
	}
}

func (builder *chunkBuilder) hasText() bool {
	return builder.text != ""
}

func (builder *chunkBuilder) appendSeparator() {
	builder.text += "\n"
}

func (builder *chunkBuilder) appendText(text string) {
	builder.text += text
}

func (builder *chunkBuilder) addPiece(piece Piece) {
	builder.pieces = append(builder.pieces, piece)
	if piece.Metadata != nil {
		builder.metadata = append(builder.metadata, *piece.Metadata)
	}
}

func (builder *chunkBuilder) markIncomplete(incomplete bool) {
	if incomplete {
		builder.coverageComplete = false
	}
}

func (builder *chunkBuilder) addPath(path string) {
	if path == "" {
		return
	}
	if _, ok := builder.pathSeen[path]; ok {
		return
	}
	builder.pathSeen[path] = struct{}{}
	builder.paths = append(builder.paths, path)
}

func (builder *chunkBuilder) build() (Chunk, bool) {
	if builder.text == "" {
		return Chunk{
			RelationshipReview: false,
			Index:              0,
			Total:              0,
			Text:               "",
			Pieces:             nil,
			Paths:              nil,
			Metadata:           nil,
			CoverageComplete:   true,
		}, false
	}
	return Chunk{
		RelationshipReview: false,
		Index:              0,
		Total:              0,
		Text:               builder.text,
		Pieces:             builder.pieces,
		Paths:              builder.paths,
		Metadata:           builder.metadata,
		CoverageComplete:   builder.coverageComplete,
	}, true
}

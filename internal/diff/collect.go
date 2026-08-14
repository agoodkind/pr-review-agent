package diff

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// FileContext is one changed file with patch metadata and current content.
type FileContext struct {
	Path              string
	Status            string
	Patch             string
	CurrentContent    string
	ChangedRightLines map[int]struct{}
	ChangedRightHunks map[int]int
	CoverageComplete  bool
}

// ReviewInput is the collected pull request context for one review pass.
type ReviewInput struct {
	PullRequest githubapp.PullRequest
	Files       []FileContext
}

// Chunk is one model input slice with complete hunks from one or more files.
type Chunk struct {
	Index            int
	Total            int
	Text             string
	Paths            []string
	CoverageComplete bool
}

// Source loads changed files and repository content for diff collection.
type Source interface {
	ListChangedFiles(context.Context, int64, domain.Repository, int) ([]githubapp.ChangedFile, error)
	GetFile(context.Context, int64, domain.Repository, string, domain.HeadSHA) ([]byte, error)
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

	sort.Slice(changedFiles, func(left, right int) bool {
		return changedFiles[left].Path < changedFiles[right].Path
	})

	files := make([]FileContext, 0, len(changedFiles))
	for _, changedFile := range changedFiles {
		files = append(files, collector.collectFile(ctx, ref, pullRequest, changedFile))
	}

	return ReviewInput{
		PullRequest: pullRequest,
		Files:       files,
	}, nil
}

func (collector *Collector) collectFile(
	ctx context.Context,
	ref domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	changedFile githubapp.ChangedFile,
) FileContext {
	fileContext := FileContext{
		Path:              changedFile.Path,
		Status:            changedFile.Status,
		Patch:             changedFile.Patch,
		CurrentContent:    "",
		ChangedRightLines: map[int]struct{}{},
		ChangedRightHunks: map[int]int{},
		CoverageComplete:  true,
	}

	if isBinaryFile(changedFile) {
		fileContext.CoverageComplete = false
		return fileContext
	}
	if !changedFile.PatchPresent {
		fileContext.CoverageComplete = false
		return fileContext
	}

	changedLines, changedHunks, err := ChangedRightLines(changedFile.Patch)
	if err != nil {
		fileContext.CoverageComplete = false
		return fileContext
	}
	fileContext.ChangedRightLines = changedLines
	fileContext.ChangedRightHunks = changedHunks

	parsed, err := parsePatch(changedFile.Patch)
	if err != nil || !parsed.complete {
		fileContext.CoverageComplete = false
	}

	if changedFile.Status == "removed" {
		return fileContext
	}

	content, err := collector.source.GetFile(
		ctx,
		ref.InstallationID,
		ref.Repository,
		changedFile.Path,
		pullRequest.Head,
	)
	if err != nil {
		fileContext.CoverageComplete = false
		return fileContext
	}
	fileContext.CurrentContent = string(content)
	return fileContext
}

func isBinaryFile(changedFile githubapp.ChangedFile) bool {
	if changedFile.Status == "binary" {
		return true
	}
	return strings.Contains(changedFile.Patch, "Binary files ")
}

func fileLooksBinary(status string, patch string) bool {
	if status == "binary" {
		return true
	}
	return strings.Contains(patch, "Binary files ")
}

// ChunkInput splits review input into deterministic chunks no larger than maxSize bytes.
func ChunkInput(input ReviewInput, maxSize int) ([]Chunk, error) {
	if maxSize < 1 {
		maxSize = 1
	}

	type pendingPiece struct {
		path             string
		text             string
		coverageComplete bool
	}

	pieces := make([]pendingPiece, 0)
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
			text := formatHunkChunk(file.Path, file.Status, file.CurrentContent, hunk)
			coverageComplete := file.CoverageComplete
			if len(text) > maxSize {
				text = formatOversizedHunkChunk(file.Path, maxSize)
				coverageComplete = false
			}
			pieces = append(pieces, pendingPiece{
				path:             file.Path,
				text:             text,
				coverageComplete: coverageComplete,
			})
		}
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
		if current.hasText() && currentSize+1+len(piece.text) > maxSize {
			flush()
		}
		if current.hasText() {
			current.appendSeparator()
			currentSize++
		}
		current.appendText(piece.text)
		currentSize += len(piece.text)
		current.markIncomplete(!piece.coverageComplete)
		current.addPath(piece.path)
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
	paths            []string
	coverageComplete bool
	pathSeen         map[string]struct{}
}

func newChunkBuilder() *chunkBuilder {
	return &chunkBuilder{
		text:             "",
		paths:            make([]string, 0),
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

func (builder *chunkBuilder) markIncomplete(incomplete bool) {
	if incomplete {
		builder.coverageComplete = false
	}
}

func (builder *chunkBuilder) addPath(path string) {
	if _, ok := builder.pathSeen[path]; ok {
		return
	}
	builder.pathSeen[path] = struct{}{}
	builder.paths = append(builder.paths, path)
}

func (builder *chunkBuilder) build() (Chunk, bool) {
	if builder.text == "" {
		return Chunk{
			Index:            0,
			Total:            0,
			Text:             "",
			Paths:            nil,
			CoverageComplete: true,
		}, false
	}
	return Chunk{
		Index:            0,
		Total:            0,
		Text:             builder.text,
		Paths:            builder.paths,
		CoverageComplete: builder.coverageComplete,
	}, true
}

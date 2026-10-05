package diff

import (
	"fmt"
)

// PromptSizeError reports a local size refusal before any provider request.
type PromptSizeError struct {
	Actual int
	Limit  int
}

// Error reports the measured payload and its configured limit.
func (failure *PromptSizeError) Error() string {
	return fmt.Sprintf("rendered review input requires %d bytes; the configured maximum is %d", failure.Actual, failure.Limit)
}

// BoundChunks measures the rendered input after reducing surplus file context.
func BoundChunks(chunks []Chunk, files []FileContext, maximumBytes int, measure func(Chunk) (int, error)) ([]Chunk, error) {
	fileIndex := make(map[string]FileContext, len(files))
	for _, file := range files {
		fileIndex[file.Path] = file
	}
	current := chunks
	for {
		next := make([]Chunk, 0, len(current))
		for index, chunk := range current {
			chunk.Index, chunk.Total = index+1, len(current)
			bounded, err := boundChunk(chunk, fileIndex, maximumBytes, measure)
			if err != nil {
				return nil, err
			}
			next = append(next, bounded...)
		}
		if len(next) == len(current) {
			return next, nil
		}
		current = next
	}
}

func boundChunk(chunk Chunk, files map[string]FileContext, maximumBytes int, measure func(Chunk) (int, error)) ([]Chunk, error) {
	size, err := measure(chunk)
	if err != nil {
		return nil, err
	}
	if size <= maximumBytes {
		return []Chunk{chunk}, nil
	}
	first, second, canSplit := chunk.Split()
	if canSplit {
		left, err := boundChunk(first, files, maximumBytes, measure)
		if err != nil {
			return nil, err
		}
		right, err := boundChunk(second, files, maximumBytes, measure)
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil
	}
	if len(chunk.Pieces) != 1 {
		return nil, &PromptSizeError{Actual: size, Limit: maximumBytes}
	}
	piece := chunk.Pieces[0]
	if piece.Metadata != nil {
		return oversizedChunk(chunk, maximumBytes, measure)
	}
	file, found := files[piece.Path]
	if !found {
		return nil, &PromptSizeError{Actual: size, Limit: maximumBytes}
	}
	parsed, err := parsePatch(piece.Patch)
	if err != nil {
		return nil, err
	}
	if len(parsed.hunks) != 1 || !parsed.complete {
		return nil, &PromptSizeError{Actual: size, Limit: maximumBytes}
	}
	hunk := parsed.hunks[0]
	minimumContext := boundedCurrentContent(file.CurrentContent, hunk.coordinates, 1)
	minimumText := formatHunkChunk(piece.Path, file.Status, "", hunk, 0)
	lower := len(minimumText) + len(minimumContext)
	upper := len(piece.Text)
	piece.Text = formatHunkChunk(piece.Path, file.Status, file.CurrentContent, hunk, lower)
	best := chunkFromPieces([]Piece{piece}, chunk.Index, chunk.Total)
	minimumSize, err := measure(best)
	if err != nil {
		return nil, err
	}
	if minimumSize > maximumBytes {
		return oversizedChunk(chunk, maximumBytes, measure)
	}
	for lower <= upper {
		middle := lower + (upper-lower)/2
		piece.Text = formatHunkChunk(piece.Path, file.Status, file.CurrentContent, hunk, middle)
		candidate := chunkFromPieces([]Piece{piece}, chunk.Index, chunk.Total)
		candidateSize, err := measure(candidate)
		if err != nil {
			return nil, err
		}
		if candidateSize <= maximumBytes {
			best = candidate
			lower = middle + 1
		} else {
			upper = middle - 1
		}
	}
	return []Chunk{best}, nil
}

func oversizedChunk(chunk Chunk, maximumBytes int, measure func(Chunk) (int, error)) ([]Chunk, error) {
	piece := chunk.Pieces[0]
	piece.Text = formatOversizedHunkChunk(piece.Path, maximumBytes)
	if piece.Metadata != nil {
		piece.Text = formatOversizedMetadataSource(*piece.Metadata, maximumBytes)
	}
	piece.CoverageComplete = false
	piece.Oversized = true
	bounded := chunkFromPieces([]Piece{piece}, chunk.Index, chunk.Total)
	size, err := measure(bounded)
	if err != nil {
		return nil, err
	}
	if size > maximumBytes {
		return nil, &PromptSizeError{Actual: size, Limit: maximumBytes}
	}
	return []Chunk{bounded}, nil
}

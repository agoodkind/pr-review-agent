package diff

import (
	"fmt"
	"strings"
)

type patchRow struct {
	text string
	old  int
	new  int
}

// splitHunk preserves coordinates and keeps newline markers with their source row.
func splitHunk(hunk parsedHunk) (parsedHunk, parsedHunk, bool) {
	var empty parsedHunk
	rows, ok := completeHunkRows(hunk)
	if !ok || len(rows) < 2 {
		return empty, empty, false
	}
	middle := balancedRowBoundary(rows)
	oldLine := hunk.coordinates.oldStart
	if hunk.coordinates.oldCount == 0 {
		oldLine++
	}
	newLine := hunk.coordinates.newStart
	if hunk.coordinates.newCount == 0 {
		newLine++
	}
	first := hunkFromRows(rows[:middle], oldLine, newLine)
	second := hunkFromRows(rows[middle:], oldLine+first.coordinates.oldCount, newLine+first.coordinates.newCount)
	return first, second, true
}

func completeHunkRows(hunk parsedHunk) ([]patchRow, bool) {
	rows := make([]patchRow, 0)
	oldCount, newCount := 0, 0
	for _, line := range strings.Split(hunk.text, "\n")[1:] {
		if line == `\ No newline at end of file` {
			if len(rows) == 0 {
				return nil, false
			}
			rows[len(rows)-1].text += "\n" + line
			continue
		}
		if line == "" {
			continue
		}
		row := patchRow{text: line, old: 0, new: 0}
		switch line[0] {
		case ' ':
			row.old, row.new = 1, 1
		case '-':
			row.old = 1
		case '+':
			row.new = 1
		default:
			return nil, false
		}
		oldCount += row.old
		newCount += row.new
		rows = append(rows, row)
	}
	return rows, oldCount == hunk.coordinates.oldCount && newCount == hunk.coordinates.newCount
}

func balancedRowBoundary(rows []patchRow) int {
	bytes := 0
	for _, row := range rows {
		bytes += len(row.text) + 1
	}
	remaining := bytes / 2
	for index, row := range rows[:len(rows)-1] {
		remaining -= len(row.text) + 1
		if remaining <= 0 {
			return index + 1
		}
	}
	return len(rows) - 1
}

func hunkFromRows(rows []patchRow, oldLine, newLine int) parsedHunk {
	coordinates := hunkHeader{oldStart: oldLine, oldCount: 0, newStart: newLine, newCount: 0}
	lines := make([]string, 0, len(rows)+1)
	for _, row := range rows {
		coordinates.oldCount += row.old
		coordinates.newCount += row.new
	}
	if coordinates.oldCount == 0 {
		coordinates.oldStart--
	}
	if coordinates.newCount == 0 {
		coordinates.newStart--
	}
	header := fmt.Sprintf("@@ -%d,%d +%d,%d @@", coordinates.oldStart, coordinates.oldCount, coordinates.newStart, coordinates.newCount)
	lines = append(lines, header)
	for _, row := range rows {
		lines = append(lines, row.text)
	}
	return parsedHunk{header: header, text: strings.Join(lines, "\n"), coordinates: coordinates}
}

func fileHunkPieces(file *FileContext, hunk parsedHunk, maximumBytes int) []Piece {
	text := formatFileHunkChunk(file, hunk, maximumBytes)
	contentMatches := file.CurrentContent == "" || ValidatePatchContent(hunk.text, file.CurrentContent) == nil
	if len(text) > maximumBytes && contentMatches {
		first, second, ok := splitHunk(hunk)
		if ok {
			return append(fileHunkPieces(file, first, maximumBytes), fileHunkPieces(file, second, maximumBytes)...)
		}
	}
	oversized := len(text) > maximumBytes
	if oversized {
		text = formatOversizedHunkChunk(file.Path, maximumBytes)
	}
	return []Piece{{
		Path: file.Path, Text: text, Patch: hunk.text, Metadata: nil,
		Header: hunkCoordinates(hunk.header), CoverageComplete: file.CoverageComplete && !oversized,
		Oversized: oversized, fileContext: file,
	}}
}

func splitFilePiece(piece Piece) (Piece, Piece, bool) {
	var empty Piece
	if piece.Metadata != nil || piece.Oversized || !piece.CoverageComplete {
		return empty, empty, false
	}
	parsed, err := parsePatch(piece.Patch)
	if err != nil || !parsed.complete || len(parsed.hunks) != 1 {
		return empty, empty, false
	}
	first, second, ok := splitHunk(parsed.hunks[0])
	if !ok {
		return empty, empty, false
	}
	return pieceFromHunk(piece, first), pieceFromHunk(piece, second), true
}

func pieceFromHunk(original Piece, hunk parsedHunk) Piece {
	piece := original
	piece.Patch = hunk.text
	piece.Header = hunkCoordinates(hunk.header)
	if original.fileContext != nil {
		piece.Text = formatFileHunkChunk(original.fileContext, hunk, len(original.Text)/2)
		return piece
	}
	piece.Text = formatHunkChunk(piece.Path, "", "", hunk, len(original.Text)/2)
	return piece
}

func formatFileHunkChunk(file *FileContext, hunk parsedHunk, maximumBytes int) string {
	metadata := ""
	if file.sourceMetadata != "" {
		metadata = file.sourceMetadata + "\n\n"
	}
	return metadata + formatHunkChunk(file.Path, file.Status, file.CurrentContent, hunk, maximumBytes-len(metadata))
}

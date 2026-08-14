// Package diff collects pull request patches and chunks them for review.
package diff

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var hunkHeaderPattern = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

type parsedPatch struct {
	changedLines map[int]struct{}
	lineHunks    map[int]int
	hunks        []parsedHunk
	complete     bool
}

type parsedHunk struct {
	header string
	text   string
}

type hunkHeader struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
}

type hunkParser struct {
	result       *parsedPatch
	hunkIndex    int
	hunkLines    []string
	hunkHeader   string
	oldLine      int
	newLine      int
	oldRemaining int
	newRemaining int
	inHunk       bool
}

// ChangedRightLines parses a patch and returns added lines with their hunk identities.
func ChangedRightLines(patch string) (map[int]struct{}, map[int]int, error) {
	result, err := parsePatch(patch)
	if err != nil {
		return nil, nil, err
	}
	return result.changedLines, result.lineHunks, nil
}

// ValidRange reports whether every line in the inclusive range is a changed right-side line
// from the same unified diff hunk.
func ValidRange(changed map[int]struct{}, hunks map[int]int, startLine, endLine int) bool {
	if startLine < 1 || endLine < startLine {
		return false
	}
	if endLine == startLine {
		_, ok := changed[startLine]
		return ok
	}

	if hunks == nil {
		return false
	}

	firstHunk, ok := hunks[startLine]
	if !ok {
		return false
	}
	for line := startLine; line <= endLine; line++ {
		if _, exists := changed[line]; !exists {
			return false
		}
		if hunks[line] != firstHunk {
			return false
		}
	}
	return true
}

func parsePatch(patch string) (parsedPatch, error) {
	lines := strings.Split(patch, "\n")
	result := parsedPatch{
		changedLines: make(map[int]struct{}),
		lineHunks:    make(map[int]int),
		hunks:        make([]parsedHunk, 0),
		complete:     true,
	}
	parser := hunkParser{
		result:       &result,
		hunkIndex:    -1,
		hunkLines:    nil,
		hunkHeader:   "",
		oldLine:      0,
		newLine:      0,
		oldRemaining: 0,
		newRemaining: 0,
		inHunk:       false,
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			parser.finishHunk()
			header, err := parseHunkHeader(line)
			if err != nil {
				return parsedPatch{}, err
			}
			parser.startHunk(line, header)
			continue
		}
		parser.applyLine(line)
	}

	parser.finishHunk()
	return result, nil
}

func parseHunkHeader(line string) (hunkHeader, error) {
	matches := hunkHeaderPattern.FindStringSubmatch(line)
	if matches == nil {
		return hunkHeader{}, errors.New("malformed hunk header")
	}

	oldStart, err := strconv.Atoi(matches[1])
	if err != nil {
		return hunkHeader{}, errors.New("malformed hunk header")
	}
	oldCount := 1
	if matches[2] != "" {
		oldCount, err = strconv.Atoi(matches[2])
		if err != nil {
			return hunkHeader{}, errors.New("malformed hunk header")
		}
	}
	newStart, err := strconv.Atoi(matches[3])
	if err != nil {
		return hunkHeader{}, errors.New("malformed hunk header")
	}
	newCount := 1
	if matches[4] != "" {
		newCount, err = strconv.Atoi(matches[4])
		if err != nil {
			return hunkHeader{}, errors.New("malformed hunk header")
		}
	}
	if oldCount == 0 && newCount == 0 {
		return hunkHeader{}, errors.New("malformed hunk header")
	}

	return hunkHeader{
		oldStart: oldStart,
		oldCount: oldCount,
		newStart: newStart,
		newCount: newCount,
	}, nil
}

func (parser *hunkParser) startHunk(line string, header hunkHeader) {
	parser.hunkIndex++
	parser.hunkHeader = line
	parser.hunkLines = []string{line}
	parser.inHunk = true
	parser.oldLine = header.oldStart
	parser.newLine = header.newStart
	parser.oldRemaining = header.oldCount
	parser.newRemaining = header.newCount
}

func (parser *hunkParser) finishHunk() {
	if !parser.inHunk {
		return
	}
	if parser.oldRemaining != 0 || parser.newRemaining != 0 {
		parser.result.complete = false
	}
	parser.result.hunks = append(parser.result.hunks, parsedHunk{
		header: parser.hunkHeader,
		text:   strings.Join(parser.hunkLines, "\n"),
	})
	parser.inHunk = false
	parser.hunkLines = nil
}

func (parser *hunkParser) applyLine(line string) {
	if !parser.inHunk {
		return
	}

	parser.hunkLines = append(parser.hunkLines, line)
	if line == `\ No newline at end of file` {
		return
	}
	if line == "" {
		if parser.oldRemaining > 0 || parser.newRemaining > 0 {
			parser.result.complete = false
		}
		return
	}

	switch {
	case strings.HasPrefix(line, " "):
		parser.applyContextLine()
	case strings.HasPrefix(line, "-"):
		parser.applyDeletionLine()
	case strings.HasPrefix(line, "+"):
		parser.applyAdditionLine()
	default:
		parser.result.complete = false
	}
}

func (parser *hunkParser) applyContextLine() {
	if parser.oldRemaining == 0 || parser.newRemaining == 0 {
		parser.result.complete = false
		return
	}
	parser.oldLine++
	parser.newLine++
	parser.oldRemaining--
	parser.newRemaining--
}

func (parser *hunkParser) applyDeletionLine() {
	if parser.oldRemaining == 0 {
		parser.result.complete = false
		return
	}
	parser.oldLine++
	parser.oldRemaining--
}

func (parser *hunkParser) applyAdditionLine() {
	if parser.newRemaining == 0 {
		parser.result.complete = false
		return
	}
	parser.result.changedLines[parser.newLine] = struct{}{}
	parser.result.lineHunks[parser.newLine] = parser.hunkIndex
	parser.newLine++
	parser.newRemaining--
}

func formatHunkChunk(path string, status string, content string, hunk parsedHunk) string {
	var builder strings.Builder
	builder.WriteString("File: ")
	builder.WriteString(path)
	builder.WriteString("\nStatus: ")
	builder.WriteString(status)
	builder.WriteString("\n\nCurrent content:\n")
	builder.WriteString(content)
	builder.WriteString("\n\nDiff hunk:\n")
	builder.WriteString(hunk.text)
	return builder.String()
}

func formatOversizedHunkChunk(path string, maxSize int) string {
	return fmt.Sprintf(
		"File: %s\n[coverage incomplete: hunk exceeds maximum chunk size %d bytes]",
		path,
		maxSize,
	)
}

package diff

import (
	"errors"
	"strings"
)

const noFinalNewlineMarker = `\ No newline at end of file`

var (
	errIncompleteReconstructedPatch = errors.New("reconstructed patch coverage is incomplete")
	errPatchOldCoordinates          = errors.New("reconstructed patch old coordinates do not match base content")
	errPatchNewCoordinates          = errors.New("reconstructed patch new coordinates do not match result content")
	errPatchBaseRow                 = errors.New("reconstructed patch row does not match base content")
	errPatchFinalNewline            = errors.New("reconstructed patch places content after a missing final newline")
	errPatchHeadContent             = errors.New("reconstructed patch does not reproduce pinned head content")
	errPatchRow                     = errors.New("reconstructed patch contains an invalid row")
)

func validatePatchTransformation(parsed parsedPatch, previous, current string) error {
	baseLines := strings.SplitAfter(previous, "\n")
	if baseLines[len(baseLines)-1] == "" {
		baseLines = baseLines[:len(baseLines)-1]
	}

	result := make([]string, 0, len(baseLines))
	oldPosition := 0

	for _, hunk := range parsed.hunks {
		oldStart := hunkStartIndex(hunk.coordinates.oldStart, hunk.coordinates.oldCount)
		newStart := hunkStartIndex(hunk.coordinates.newStart, hunk.coordinates.newCount)

		if oldStart < oldPosition || oldStart > len(baseLines) {
			return errPatchOldCoordinates
		}

		result = append(result, baseLines[oldPosition:oldStart]...)
		if newStart != len(result) {
			return errPatchNewCoordinates
		}

		rows, err := transformationRows(hunk.text)
		if err != nil {
			return err
		}

		oldPosition = oldStart

		for _, row := range rows {
			if row[0] != '+' {
				if oldPosition >= len(baseLines) || baseLines[oldPosition] != row[1:] {
					return errPatchBaseRow
				}

				oldPosition++
			}

			if row[0] != '-' {
				result = append(result, row[1:])
			}
		}
	}

	result = append(result, baseLines[oldPosition:]...)

	for _, line := range result[:max(0, len(result)-1)] {
		if !strings.HasSuffix(line, "\n") {
			return errPatchFinalNewline
		}
	}

	if strings.Join(result, "") != current {
		return errPatchHeadContent
	}

	return nil
}

func hunkStartIndex(start, count int) int {
	if count == 0 {
		return start
	}

	return start - 1
}

func transformationRows(text string) ([]string, error) {
	lines := strings.Split(text, "\n")[1:]
	rows := make([]string, 0, len(lines))

	for index := 0; index < len(lines); index++ {
		line := lines[index]
		if line == "" && index == len(lines)-1 {
			continue
		}

		if line == "" || (line[0] != ' ' && line[0] != '-' && line[0] != '+') {
			return nil, errPatchRow
		}

		row := line + "\n"
		if index+1 < len(lines) && lines[index+1] == noFinalNewlineMarker {
			row = line
			index++
		}

		rows = append(rows, row)
	}

	return rows, nil
}

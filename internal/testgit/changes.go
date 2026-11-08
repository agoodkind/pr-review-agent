package testgit

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var hunkCoordinates = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// PatchContents constructs fixture file bytes from complete patch rows.
//
// Context and deletion rows define the old bytes. Context and insertion rows
// define the new bytes at their stated line numbers.
//
// Example:
//
//	before, after := PatchContents(patch)
func PatchContents(patch string) (string, string) {
	before, after := make(map[int]string), make(map[int]string)
	oldLine, newLine := 0, 0

	for row := range strings.SplitSeq(patch, "\n") {
		if coordinates := hunkCoordinates.FindStringSubmatch(row); coordinates != nil {
			oldLine, _ = strconv.Atoi(coordinates[1])
			newLine, _ = strconv.Atoi(coordinates[2])

			continue
		}

		if row == "" {
			continue
		}

		switch row[0] {
		case ' ':
			before[oldLine], after[newLine] = row[1:], row[1:]
			oldLine++
			newLine++
		case '-':
			before[oldLine] = row[1:]
			oldLine++
		case '+':
			after[newLine] = row[1:]
			newLine++
		}
	}

	return renderPatchLines(before), renderPatchLines(after)
}

func renderPatchLines(lines map[int]string) string {
	maximum := 0
	for line := range lines {
		maximum = max(maximum, line)
	}

	var content strings.Builder

	for line := 1; line <= maximum; line++ {
		content.WriteString(lines[line])
		content.WriteByte('\n')
	}

	return content.String()
}

// CommitReview creates base, changed, and corrected fixture commits.
//
// Rebuilding an existing fixture creates an orphan branch. Corrections append
// text only to files present in the changed tree.
//
// Example:
//
//	base, head, corrected := CommitReview(t, directory, generation, before, after, corrections)
func CommitReview(
	t *testing.T,
	directory string,
	generation int,
	before map[string]string,
	after map[string]string,
	corrections map[string]string,
) (string, string, string) {
	t.Helper()

	if generation > 0 {
		Command(t, directory, "checkout", "--orphan", "fixture-update-"+strconv.Itoa(generation))
		Command(t, directory, "rm", "-r", "-f", "--ignore-unmatch", ".")
	}

	base := Commit(t, directory, "Create review base", before, nil)
	head := Commit(t, directory, "Change review sources", after, nil)

	for path, correction := range corrections {
		if _, exists := after[path]; exists {
			after[path] += correction
		}
	}

	corrected := Commit(t, directory, "Correct review sources", after, nil)

	return base, head, corrected
}

// ContentAPIRequest identifies the repository-content endpoints a Git fixture
// must refuse.
//
// Metadata and publication requests remain available to the fixture server.
//
// Example:
//
//	forbidden := ContentAPIRequest(request)
func ContentAPIRequest(request *http.Request) bool {
	endpoints := []string{
		"/contents/", "/git/blobs/", "/git/trees/", "/tarball/", "/compare/", "/search/code",
	}
	for _, endpoint := range endpoints {
		if strings.Contains(request.URL.Path, endpoint) {
			return true
		}
	}

	return strings.HasSuffix(request.URL.Path, "/files")
}

package githubapp_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
)

const (
	gitBinaryPath          = "oversized.bin"
	gitBinaryBytes         = 16*1024*1024 + 1
	gitBinaryProbeOffset   = 7999
	gitBinarySourceContent = "package fixture\nvar Value = 3\n"
)

// TestGitCollectorSkipsOversizedBinaryChanges verifies that binary additions
// and deletions do not prevent ordinary source review.
func TestGitCollectorSkipsOversizedBinaryChanges(t *testing.T) {
	for _, removed := range []bool{false, true} {
		name := "addition"
		if removed {
			name = "deletion"
		}

		t.Run(
			name,
			func(t *testing.T) {
				fixture := newGitFixture(t)
				prepareOversizedBinaryChange(t, fixture, removed)
				ctx, pullRequest, ref := openGitFixtureReview(t, fixture)

				input, err := diff.NewCollector(fixture.client).Collect(ctx, ref, pullRequest)
				if err != nil {
					t.Fatal(err)
				}

				assertOversizedBinaryReview(t, input)
				fixture.mutex.Lock()
				defer fixture.mutex.Unlock()

				if fixture.contentRequests != 0 {
					t.Fatalf("binary review content API requests=%d", fixture.contentRequests)
				}
			},
		)
	}
}

func prepareOversizedBinaryChange(t *testing.T, fixture *gitFixture, removed bool) {
	t.Helper()

	data := bytes.Repeat([]byte{'b'}, gitBinaryBytes)
	data[gitBinaryProbeOffset] = 0

	writeError := os.WriteFile(filepath.Join(fixture.directory, gitBinaryPath), data, 0o600)
	if writeError != nil {
		t.Fatal(writeError)
	}

	gitCommand(t, fixture.directory, "add", gitBinaryPath)

	if removed {
		gitCommand(t, fixture.directory, "commit", "-m", "Add the original binary fixture")
		fixture.base = domain.HeadSHA(gitCommand(t, fixture.directory, "rev-parse", "HEAD"))
		gitCommand(t, fixture.directory, "rm", gitBinaryPath)
	}

	writeGitFixtureFile(t, fixture.directory, gitFixtureSourcePath, gitBinarySourceContent)
	gitCommand(t, fixture.directory, "add", gitFixtureSourcePath)
	gitCommand(t, fixture.directory, "commit", "-m", "Change binary and ordinary source fixtures")
	fixture.head = domain.HeadSHA(gitCommand(t, fixture.directory, "rev-parse", "HEAD"))
}

func assertOversizedBinaryReview(t *testing.T, input diff.ReviewInput) {
	t.Helper()

	binaryFound, sourceFound := false, false

	for _, file := range input.Files {
		switch file.Path {
		case gitBinaryPath:
			binaryFound = true

			if file.Status != "binary" || file.Gap != diff.CoverageGapBinary ||
				file.CurrentContent != "" {
				t.Fatalf("oversized binary context = %+v", file)
			}
		case gitFixtureSourcePath:
			sourceFound = true

			_, anchored := file.ChangedRightLines[2]
			if !anchored || file.CurrentContent != gitBinarySourceContent {
				t.Fatalf("ordinary source context = %+v", file)
			}
		}
	}

	if !binaryFound || !sourceFound {
		t.Fatal("The review must retain both the binary exclusion and ordinary source")
	}

	chunks, err := diff.ChunkInput(input, config.MaximumPromptBytes)
	if err != nil {
		t.Fatal(err)
	}

	sourceChunk := false

	for _, chunk := range chunks {
		if slices.Contains(chunk.Paths, gitBinaryPath) {
			t.Fatal("The review chunks must exclude binary content")
		}

		sourceChunk = sourceChunk || slices.Contains(chunk.Paths, gitFixtureSourcePath)
	}

	if !sourceChunk {
		t.Fatal("The review chunks must include the ordinary changed source")
	}
}

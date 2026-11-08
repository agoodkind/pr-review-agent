package diff_test

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
)

const (
	recoveryAbsentPatch    = "absent"
	recoveryModifiedStatus = "modified"
	recoveryCRLFContent    = "first\r\nlast\r\n"
)

// TestCollectorRecoversEquivalentDiffWithDifferentCounts checks source
// content and finding anchors against immutable Git objects.
func TestCollectorRecoversEquivalentDiffWithDifferentCounts(t *testing.T) {
	before := strings.Repeat("previous line\n", 60) + "shared middle\n" + strings.Repeat("previous line\n", 60)
	after := strings.Repeat("updated line\n", 60) + "shared middle\n" + strings.Repeat("updated line\n", 60)
	source := newRecoveryPair(t, before, after)

	comparison, err := source.Compare(t.Context(), 1, recoveryRef(source.head).Repository, source.base, source.head)
	if err != nil {
		t.Fatal(err)
	}

	if len(comparison.Files) != 1 || comparison.Files[0].Additions != 120 || comparison.Files[0].Deletions != 120 {
		t.Fatalf("Git must retain the shared middle line: %+v", comparison.Files)
	}

	input, err := diff.NewCollector(source).Collect(t.Context(), recoveryRef(source.head), recoveryPullRequest(source))
	if err != nil {
		t.Fatal(err)
	}

	expected := make(map[int]string)
	for index, line := range strings.Split(strings.TrimSuffix(after, "\n"), "\n") {
		expected[index+1] = line
	}

	assertRecoveredRepositoryFiles(t, source, input, map[string]map[int]string{"changed.txt": expected})

	if len(input.Files[0].ChangedRightLines) != comparison.Files[0].Additions+1 {
		t.Fatal("The reconstructed patch must exercise different counts from the Git patch.")
	}

	assertRecoveredTransformation(t, before, after, input.Files[0].Patch)
}

// TestCollectorRecoversExactLineEndings checks complete content after
// changes to empty files, line endings, and final newlines.
func TestCollectorRecoversExactLineEndings(t *testing.T) {
	for _, contents := range []struct {
		name   string
		before string
		after  string
	}{
		{name: "empty base", before: "", after: "first\r\nlast"},
		{name: "add final newline", before: "first\r\nlast", after: recoveryCRLFContent},
		{name: "replace LF with CRLF", before: "first\nlast\n", after: recoveryCRLFContent},
		{name: "empty head", before: recoveryCRLFContent, after: ""},
	} {
		t.Run(contents.name, func(t *testing.T) {
			source := newRecoveryPair(t, contents.before, contents.after)

			input, err := diff.NewCollector(source).Collect(t.Context(), recoveryRef(source.head), recoveryPullRequest(source))
			if err != nil {
				t.Fatal(err)
			}

			if len(input.Files) != 1 || !input.Files[0].CoverageComplete || input.Files[0].CurrentContent != contents.after {
				t.Fatal("Recovery must retain the complete head bytes.")
			}

			assertRecoveredTransformation(t, contents.before, contents.after, input.Files[0].Patch)
		})
	}
}

func newRecoveryPair(t *testing.T, before, after string) *repositorySource {
	t.Helper()
	directory := t.TempDir()
	largeGitCommand(t, directory, "init", "--quiet")
	writeRecoveryFile(t, directory, "changed.txt", before)
	largeGitCommand(t, directory, "add", "--all")
	largeGitCommit(t, directory, "Create recovery source")
	base := domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", "HEAD")))
	writeRecoveryFile(t, directory, "changed.txt", after)
	largeGitCommand(t, directory, "add", "--all")
	largeGitCommit(t, directory, "Update recovery source")
	head := domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", "HEAD")))

	return &repositorySource{
		directory: directory, base: base, head: head,
		patchMode: recoveryAbsentPatch, comparisons: 0, contentReads: nil,
	}
}

func assertRecoveredTransformation(t *testing.T, before, after, patch string) {
	t.Helper()

	_, hunks, found := strings.Cut(patch, "\n@@")
	if !found {
		t.Fatal("The recovered patch must include a hunk.")
	}

	assertLargeGitApply(t, largeGitChange{
		before: before, after: after, status: recoveryModifiedStatus, added: nil, removed: nil,
	}, []string{"@@" + hunks})
}

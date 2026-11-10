package githubapp_test

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
)

// TestGitCacheNegotiatesNewHeadsWithoutDownloadingUnchangedObjects verifies
// object reuse across reviewed heads.
func TestGitCacheNegotiatesNewHeadsWithoutDownloadingUnchangedObjects(t *testing.T) {
	fixture := newGitFixture(t)

	unchanged := make([]byte, 1024*1024)

	_, randomError := rand.Read(unchanged)
	if randomError != nil {
		t.Fatal(randomError)
	}

	writeError := os.WriteFile(filepath.Join(fixture.directory, "unchanged.bin"), unchanged, 0o600)
	if writeError != nil {
		t.Fatal(writeError)
	}

	gitCommand(t, fixture.directory, "add", "unchanged.bin")
	gitCommand(t, fixture.directory, "commit", "-m", "Add unchanged repository object")

	first := domain.HeadSHA(gitCommand(t, fixture.directory, "rev-parse", "HEAD"))

	_, initialReadError := fixture.client.GetFile(
		t.Context(),
		77,
		fixture.repo,
		gitFixtureSourcePath,
		first,
	)
	if initialReadError != nil {
		t.Fatal(initialReadError)
	}

	fixture.mutex.Lock()
	initialBytes := fixture.gitBytes
	fixture.mutex.Unlock()
	writeGitFixtureFile(
		t,
		fixture.directory,
		gitFixtureSourcePath,
		"package fixture\nvar Value = 3\n",
	)
	gitCommand(t, fixture.directory, "add", gitFixtureSourcePath)
	gitCommand(t, fixture.directory, "commit", "-m", "Change one small source")
	second := domain.HeadSHA(gitCommand(t, fixture.directory, "rev-parse", "HEAD"))

	content, err := fixture.client.GetFile(
		t.Context(),
		77,
		fixture.repo,
		gitFixtureSourcePath,
		second,
	)
	if err != nil || string(content) != "package fixture\nvar Value = 3\n" {
		t.Fatalf("new head content = %q: %v", content, err)
	}

	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	additionalBytes := fixture.gitBytes - initialBytes
	if initialBytes < 1024*1024 || additionalBytes >= initialBytes/10 ||
		fixture.contentRequests != 0 {
		t.Fatalf(
			"initial Git bytes=%d, incremental Git bytes=%d, content API requests=%d, haves=%v",
			initialBytes,
			additionalBytes,
			fixture.contentRequests,
			fixture.haves,
		)
	}

	t.Logf(
		"initial Git bytes=%d; new-head Git bytes=%d; unchanged object bytes=%d; content API requests=0",
		initialBytes,
		additionalBytes,
		len(unchanged),
	)
}

// TestGetFileReturnsGitSubmoduleReference verifies the parent repository
// gitlink and URL.
func TestGetFileReturnsGitSubmoduleReference(t *testing.T) {
	fixture := newGitFixture(t)
	content, err := fixture.client.GetFile(
		t.Context(),
		77,
		fixture.repo,
		"lib/module",
		fixture.head,
	)

	want := "Submodule repository: https://example.invalid/nested.git\nSubproject commit " + string(
		fixture.root,
	) + "\n"
	if err != nil || string(content) != want {
		t.Fatalf("gitlink content = %q, want %q: %v", content, want, err)
	}
}

// TestGetFilePreservesTheExistingContentLimit verifies the existing
// individual file byte limit.
func TestGetFilePreservesTheExistingContentLimit(t *testing.T) {
	fixture := newGitFixture(t)
	writeGitFixtureFile(t, fixture.directory, "large.txt", strings.Repeat("x", 16*1024*1024+1))
	gitCommand(t, fixture.directory, "add", "large.txt")
	gitCommand(t, fixture.directory, "commit", "-m", "Add large source")
	head := domain.HeadSHA(gitCommand(t, fixture.directory, "rev-parse", "HEAD"))

	content, err := fixture.client.GetFile(t.Context(), 77, fixture.repo, "large.txt", head)
	if err == nil || len(content) != 0 ||
		!strings.Contains(err.Error(), "content response byte limit") {
		t.Fatalf("oversized content = %d bytes: %v", len(content), err)
	}

	_, comparisonError := fixture.client.Compare(t.Context(), 77, fixture.repo, fixture.head, head)
	if comparisonError == nil ||
		!strings.Contains(comparisonError.Error(), "content response byte limit") {
		t.Fatalf("oversized text comparison must preserve the content limit: %v", comparisonError)
	}
}

// TestGitCacheSeparatesInstallationsAndEvictsInactiveRepositories verifies
// identity separation and bounded cache entries.
func TestGitCacheSeparatesInstallationsAndEvictsInactiveRepositories(t *testing.T) {
	fixture := newGitFixture(t)

	for installation := int64(1); installation <= 33; installation++ {
		_, installationError := fixture.client.GetFile(
			t.Context(),
			installation,
			fixture.repo,
			gitFixtureSourcePath,
			fixture.head,
		)
		if installationError != nil {
			t.Fatal(installationError)
		}
	}

	fixture.mutex.Lock()
	before := fixture.transfers
	fixture.mutex.Unlock()

	if before != 33 {
		t.Fatalf("separate installation transfers=%d, want 33", before)
	}

	_, currentErr := fixture.client.GetFile(
		t.Context(),
		33,
		fixture.repo,
		gitFixtureSourcePath,
		fixture.head,
	)
	if currentErr != nil {
		t.Fatal(currentErr)
	}

	_, evictedErr := fixture.client.GetFile(
		t.Context(),
		1,
		fixture.repo,
		gitFixtureSourcePath,
		fixture.head,
	)
	if evictedErr != nil {
		t.Fatal(evictedErr)
	}

	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	if fixture.transfers != 34 || fixture.contentRequests != 0 {
		t.Fatalf(
			"cache transfers=%d, content API requests=%d",
			fixture.transfers,
			fixture.contentRequests,
		)
	}

	t.Log(
		"33 installation identities require 33 transfers; " +
			"the cached identity requires 0; the evicted identity requires 1",
	)
}

// TestGitComparisonBatchesMissingAncestry verifies bounded transfers through
// a long history.
func TestGitComparisonBatchesMissingAncestry(t *testing.T) {
	fixture := newGitFixture(t)

	for index := range 100 {
		writeGitFixtureFile(
			t,
			fixture.directory,
			"history.txt",
			strings.Repeat("history\n", index+1),
		)
		gitCommand(t, fixture.directory, "add", "history.txt")
		gitCommand(t, fixture.directory, "commit", "-m", "Extend feature history")
	}

	head := domain.HeadSHA(gitCommand(t, fixture.directory, "rev-parse", "HEAD"))

	comparison, err := fixture.client.Compare(t.Context(), 77, fixture.repo, fixture.base, head)
	if err != nil || comparison.MergeBase != fixture.root {
		t.Fatalf(
			"long-history merge base = %s, want %s: %v",
			comparison.MergeBase,
			fixture.root,
			err,
		)
	}

	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	if fixture.transfers > 5 || fixture.contentRequests != 0 {
		t.Fatalf(
			"100 commits required %d transfers and %d content API requests",
			fixture.transfers,
			fixture.contentRequests,
		)
	}

	t.Logf(
		"100 additional commits require %d Git transfers; content API requests=0",
		fixture.transfers,
	)
}

// TestGitComparisonCountsSourceRowsThatResembleDiffHeaders verifies complete
// local patch counts.
func TestGitComparisonCountsSourceRowsThatResembleDiffHeaders(t *testing.T) {
	fixture := newGitFixture(t)
	writeGitFixtureFile(t, fixture.directory, gitFixtureSourcePath, "package fixture\n++value\n")
	gitCommand(t, fixture.directory, "add", gitFixtureSourcePath)
	gitCommand(t, fixture.directory, "commit", "-m", "Add source with diff punctuation")
	head := domain.HeadSHA(gitCommand(t, fixture.directory, "rev-parse", "HEAD"))

	comparison, err := fixture.client.Compare(t.Context(), 77, fixture.repo, fixture.head, head)
	if err != nil {
		t.Fatal(err)
	}

	if len(comparison.Files) != 1 || comparison.Files[0].Additions != 1 ||
		comparison.Files[0].Deletions != 1 {
		t.Fatalf("complete local patch counts = %+v", comparison.Files)
	}
}

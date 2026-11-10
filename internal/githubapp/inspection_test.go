package githubapp_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
)

// TestGitTransportCollectsPinnedReviewWithoutContentAPIRequests verifies
// local collection and cached concurrent reads.

// TestGitTransportCollectsPinnedReviewWithoutContentAPIRequests verifies
// local collection and cached concurrent reads.
func TestGitTransportCollectsPinnedReviewWithoutContentAPIRequests(t *testing.T) {
	fixture := newGitFixture(t)
	client := fixture.client

	ctx, closeSnapshot, err := client.OpenRepositorySnapshot(
		t.Context(),
		77,
		fixture.repo,
		fixture.head,
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = closeSnapshot() })

	pullRequest, err := client.GetPullRequest(ctx, 77, fixture.repo, 7)
	if err != nil {
		t.Fatal(err)
	}

	ref := domain.PullRequestRef{
		InstallationID: 77,
		Repository:     fixture.repo,
		Number:         7,
		Head:           fixture.head,
	}

	input, err := diff.NewCollector(client).Collect(ctx, ref, pullRequest)
	if err != nil {
		t.Fatal(err)
	}

	assertGitCollectedFiles(t, input)

	comparison, err := client.Compare(ctx, 77, fixture.repo, fixture.base, fixture.head)
	if err != nil || comparison.MergeBase != fixture.root {
		t.Fatalf("merge base = %s, want %s: %v", comparison.MergeBase, fixture.root, err)
	}

	assertGitSpecialFiles(t, fixture, ctx, comparison)

	paths, err := client.SearchRepository(ctx, 77, fixture.repo, "var Value = 2")
	if err != nil || !slices.Equal(paths, []string{gitFixtureSourcePath}) {
		t.Fatalf("literal search = %v: %v", paths, err)
	}

	_, foreignError := client.GetFileContent(
		ctx,
		78,
		fixture.repo,
		gitFixtureSourcePath,
		fixture.head,
	)
	if foreignError == nil {
		t.Fatal("A snapshot accepted another installation")
	}

	_, unsafePathError := client.GetFileContent(ctx, 77, fixture.repo, "../source.go", fixture.head)
	if unsafePathError == nil {
		t.Fatal("A snapshot accepted a parent path")
	}

	closeErr := closeSnapshot()
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	_, closedScopeError := client.GetFileContent(
		ctx,
		77,
		fixture.repo,
		gitFixtureSourcePath,
		fixture.head,
	)
	if closedScopeError == nil {
		t.Fatal("A closed snapshot accepted a read")
	}

	fixture.mutex.Lock()
	before := fixture.transfers
	fixture.mutex.Unlock()

	var group sync.WaitGroup

	for range 4 {
		group.Go(func() {
			content, err := client.GetFileContent(
				t.Context(),
				77,
				fixture.repo,
				gitFixtureSourcePath,
				fixture.head,
			)
			if err != nil || string(content.Data) != gitFixtureSourceContent {
				t.Errorf("Repeated review = %q: %v", content.Data, err)
			}
		})
	}

	group.Wait()
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	if fixture.contentRequests != 0 || fixture.metadataRequests != 1 ||
		fixture.transfers != before {
		t.Fatalf(
			"content requests=%d, metadata requests=%d, transfers=%d, before repeated reviews=%d",
			fixture.contentRequests,
			fixture.metadataRequests,
			fixture.transfers,
			before,
		)
	}

	t.Logf(
		"content API requests=0; metadata requests=1; initial Git transfers=%d; repeated/concurrent Git transfers=0",
		before,
	)
}

// TestGitTransportPreservesCancellation verifies cancellation before
// repository acquisition.

// TestGitTransportPreservesCancellation verifies canceled acquisition.
func TestGitTransportPreservesCancellation(t *testing.T) {
	fixture := newGitFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := fixture.client.GetFileContent(
		ctx,
		77,
		fixture.repo,
		gitFixtureSourcePath,
		fixture.head,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

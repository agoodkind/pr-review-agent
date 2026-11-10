package githubapp_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

const (
	gitHistoryUnknownRevision = "ffffffffffffffffffffffffffffffffffffffff"
	gitHistoryAddedPath       = "intermediate.txt"
	gitHistoryCrisscrossPath  = "crisscross.txt"
)

func openGitFixtureReview(
	t *testing.T,
	fixture *gitFixture,
) (context.Context, githubapp.PullRequest, domain.PullRequestRef) {
	t.Helper()

	ctx, closeSnapshot, err := fixture.client.OpenRepositorySnapshot(
		t.Context(),
		77,
		fixture.repo,
		fixture.head,
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = closeSnapshot() })

	pullRequest, err := fixture.client.GetPullRequest(ctx, 77, fixture.repo, 7)
	if err != nil {
		t.Fatal(err)
	}

	ref := domain.PullRequestRef{
		InstallationID: 77,
		Repository:     fixture.repo,
		Number:         7,
		Head:           fixture.head,
	}

	return ctx, pullRequest, ref
}

// TestGitIncrementalReviewRecoversUnavailableHistory verifies full collection
// without requesting an unknown previous commit.
func TestGitIncrementalReviewRecoversUnavailableHistory(t *testing.T) {
	fixture := newGitFixture(t)
	ctx, pullRequest, ref := openGitFixtureReview(t, fixture)
	previous := domain.HeadSHA(gitHistoryUnknownRevision)

	_, err := fixture.client.CompareIncremental(
		ctx,
		ref.InstallationID,
		ref.Repository,
		previous,
		pullRequest,
	)

	unavailable, matches := errors.AsType[*githubapp.IncrementalHistoryUnavailableError](err)
	if !matches || unavailable.Revision != previous {
		t.Fatalf("unavailable history condition = %v", err)
	}

	input, err := diff.NewCollector(fixture.client).CollectRange(ctx, ref, pullRequest, previous)
	if err != nil {
		t.Fatal(err)
	}

	assertGitCollectedFiles(t, input)
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	if slices.Contains(fixture.wants, plumbing.NewHash(gitHistoryUnknownRevision)) {
		t.Fatal("The current review requested an unknown previous SHA")
	}

	if fixture.contentRequests != 0 || fixture.metadataRequests != 1 {
		t.Fatalf(
			"content API requests=%d, metadata requests=%d",
			fixture.contentRequests,
			fixture.metadataRequests,
		)
	}

	t.Log(
		"The collector completed the full current PR with zero content API requests and zero wants for the unknown SHA",
	)
}

// TestGitIncrementalReviewUsesReachableColdHistory verifies that a cold cache
// still supplies the ordinary incremental review.
func TestGitIncrementalReviewUsesReachableColdHistory(t *testing.T) {
	fixture := newGitFixture(t)
	previous := fixture.head
	writeGitFixtureFile(t, fixture.directory, gitHistoryAddedPath, "intermediate change\n")
	gitCommand(t, fixture.directory, "add", gitHistoryAddedPath)
	gitCommand(t, fixture.directory, "commit", "-m", "Add an intermediate change")
	writeGitFixtureFile(
		t,
		fixture.directory,
		gitFixtureSourcePath,
		"package fixture\nvar Value = 3\n",
	)
	gitCommand(t, fixture.directory, "add", gitFixtureSourcePath)
	gitCommand(t, fixture.directory, "commit", "-m", "Update the current source")
	fixture.head = domain.HeadSHA(gitCommand(t, fixture.directory, "rev-parse", "HEAD"))
	ctx, pullRequest, ref := openGitFixtureReview(t, fixture)

	input, err := diff.NewCollector(fixture.client).CollectRange(ctx, ref, pullRequest, previous)
	if err != nil {
		t.Fatal(err)
	}

	paths := make([]string, 0, len(input.Files))
	for _, file := range input.Files {
		paths = append(paths, file.Path)
	}

	if !slices.Equal(paths, []string{gitHistoryAddedPath, gitFixtureSourcePath}) ||
		input.MergeBase != previous {
		t.Fatalf(
			"incremental paths=%v, merge base=%s, previous=%s",
			paths,
			input.MergeBase,
			previous,
		)
	}

	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	if fixture.contentRequests != 0 || fixture.metadataRequests != 1 {
		t.Fatalf(
			"content API requests=%d, metadata requests=%d",
			fixture.contentRequests,
			fixture.metadataRequests,
		)
	}

	t.Log("The cold cache retained the reachable previous SHA and reviewed only the two new paths")
}

// TestGitIncrementalReviewPreservesAcquisitionFailures verifies that current
// Git authorization and transport failures cannot trigger history recovery.
func TestGitIncrementalReviewPreservesAcquisitionFailures(t *testing.T) {
	statuses := []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusServiceUnavailable,
	}
	for _, status := range statuses {
		t.Run(
			http.StatusText(status),
			func(t *testing.T) {
				fixture := newGitFixture(t)
				fixture.gitStatus = status
				ctx, pullRequest, ref := openGitFixtureReview(t, fixture)
				_, err := diff.NewCollector(fixture.client).CollectRange(
					ctx,
					ref,
					pullRequest,
					domain.HeadSHA(gitHistoryUnknownRevision),
				)

				_, unavailable := errors.AsType[*githubapp.IncrementalHistoryUnavailableError](err)
				if err == nil || unavailable {
					t.Fatalf("current Git status %d returned %v", status, err)
				}
			},
		)
	}
}

// TestGitIncrementalReviewPreservesCancellation verifies cancellation before
// the current legitimate history can load.
func TestGitIncrementalReviewPreservesCancellation(t *testing.T) {
	fixture := newGitFixture(t)
	ctx, pullRequest, ref := openGitFixtureReview(t, fixture)
	canceled, cancel := context.WithCancel(ctx)
	cancel()

	_, err := diff.NewCollector(fixture.client).CollectRange(
		canceled,
		ref,
		pullRequest,
		domain.HeadSHA(gitHistoryUnknownRevision),
	)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("incremental cancellation = %v", err)
	}
}

// TestGitComparisonAcceptsCrisscrossMergeBases verifies deterministic selection
// from native Git's valid best-base set and consistent review anchors.
func TestGitComparisonAcceptsCrisscrossMergeBases(t *testing.T) {
	fixture := newGitFixture(t)
	leftTree := gitCommand(t, fixture.directory, "rev-parse", string(fixture.base)+"^{tree}")
	left := gitCommand(
		t,
		fixture.directory,
		"commit-tree",
		leftTree,
		"-p",
		string(fixture.base),
		"-p",
		string(fixture.head),
		"-m",
		"Merge both branches on the left",
	)
	writeGitFixtureFile(
		t,
		fixture.directory,
		gitHistoryCrisscrossPath,
		"review this added source\n",
	)
	gitCommand(t, fixture.directory, "add", gitHistoryCrisscrossPath)
	rightTree := gitCommand(t, fixture.directory, "write-tree")
	right := gitCommand(
		t,
		fixture.directory,
		"commit-tree",
		rightTree,
		"-p",
		string(fixture.head),
		"-p",
		string(fixture.base),
		"-m",
		"Merge both branches on the right",
	)
	gitCommand(t, fixture.directory, "update-ref", "refs/heads/crisscross-left", left)
	gitCommand(t, fixture.directory, "update-ref", "refs/heads/crisscross-right", right)

	bestBases := strings.Fields(
		gitCommand(t, fixture.directory, "merge-base", "--all", left, right),
	)
	if len(bestBases) != 2 {
		t.Fatalf("native Git best bases = %v", bestBases)
	}

	fixture.base, fixture.head = domain.HeadSHA(left), domain.HeadSHA(right)
	ctx, pullRequest, ref := openGitFixtureReview(t, fixture)

	comparison, err := fixture.client.Compare(
		ctx,
		ref.InstallationID,
		ref.Repository,
		fixture.base,
		fixture.head,
	)
	if err != nil || !slices.Contains(bestBases, string(comparison.MergeBase)) {
		t.Fatalf("selected base=%s, native bases=%v: %v", comparison.MergeBase, bestBases, err)
	}

	previous, err := fixture.client.GetFile(
		ctx,
		ref.InstallationID,
		ref.Repository,
		gitFixtureSourcePath,
		comparison.MergeBase,
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := gitCommand(
		t,
		fixture.directory,
		"show",
		string(comparison.MergeBase)+":"+gitFixtureSourcePath,
	)
	if strings.TrimSpace(string(previous)) != expected {
		t.Fatal("The selected merge base did not supply the old file bytes")
	}

	input, err := diff.NewCollector(fixture.client).Collect(ctx, ref, pullRequest)
	if err != nil {
		t.Fatal(err)
	}

	assertCrisscrossAddedAnchor(t, input)

	repeated, err := fixture.client.Compare(
		ctx,
		ref.InstallationID,
		ref.Repository,
		fixture.base,
		fixture.head,
	)
	if err != nil || repeated.MergeBase != comparison.MergeBase {
		t.Fatalf(
			"repeated base=%s, first base=%s: %v",
			repeated.MergeBase,
			comparison.MergeBase,
			err,
		)
	}

	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	if fixture.contentRequests != 0 {
		t.Fatalf("crisscross content API requests=%d", fixture.contentRequests)
	}
}

func assertCrisscrossAddedAnchor(t *testing.T, input diff.ReviewInput) {
	t.Helper()

	for _, file := range input.Files {
		if file.Path != gitHistoryCrisscrossPath {
			continue
		}

		_, anchored := file.ChangedRightLines[1]
		if !anchored || !file.CoverageComplete ||
			file.CurrentContent != "review this added source\n" {
			t.Fatalf("crisscross source anchor = %+v", file)
		}

		return
	}

	t.Fatal("The crisscross review omitted its changed source")
}

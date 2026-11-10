package githubapp_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// TestGitCurrentTokenRefusalDoesNotWidenReview verifies that a one-time current
// token refusal cannot cause the legacy HTTP-status recovery.
func TestGitCurrentTokenRefusalDoesNotWidenReview(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnprocessableEntity} {
		t.Run(
			http.StatusText(status),
			func(t *testing.T) {
				fixture := newGitFixture(t)
				fixture.tokenLifetime = time.Minute
				fixture.tokenStatuses = []int{http.StatusCreated, status, http.StatusCreated}
				ctx, pullRequest, ref := openGitFixtureReview(t, fixture)
				_, err := diff.NewCollector(fixture.client).CollectRange(
					ctx,
					ref,
					pullRequest,
					domain.HeadSHA(gitHistoryUnknownRevision),
				)

				apiError, matches := errors.AsType[githubapp.APIError](err)
				if !matches || apiError.StatusCode != status {
					t.Fatalf("current token status %d returned %v", status, err)
				}

				fixture.mutex.Lock()
				defer fixture.mutex.Unlock()

				if fixture.tokenRequests != 2 || fixture.contentRequests != 0 ||
					fixture.transfers != 0 {
					t.Fatalf(
						"token requests=%d, content requests=%d, Git transfers=%d",
						fixture.tokenRequests,
						fixture.contentRequests,
						fixture.transfers,
					)
				}
			},
		)
	}
}

// TestGitUnrelatedCurrentHistoryDoesNotWidenReview verifies that the current
// comparison's HTTP-compatible 422 error remains a comparison failure.
func TestGitUnrelatedCurrentHistoryDoesNotWidenReview(t *testing.T) {
	fixture := newGitFixture(t)
	tree := gitCommand(t, fixture.directory, "rev-parse", string(fixture.head)+"^{tree}")
	unrelated := gitCommand(
		t,
		fixture.directory,
		"commit-tree",
		tree,
		"-m",
		"Create unrelated current history",
	)
	gitCommand(t, fixture.directory, "update-ref", "refs/heads/unrelated-current", unrelated)
	fixture.head = domain.HeadSHA(unrelated)
	ctx, pullRequest, ref := openGitFixtureReview(t, fixture)

	_, err := diff.NewCollector(fixture.client).CollectRange(
		ctx,
		ref,
		pullRequest,
		domain.HeadSHA(gitHistoryUnknownRevision),
	)

	apiError, matches := errors.AsType[githubapp.APIError](err)
	if !matches || apiError.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unrelated current history returned %v", err)
	}

	if strings.Contains(err.Error(), "list changed files") ||
		!strings.HasPrefix(err.Error(), "compare ") {
		t.Fatalf("The current history error entered full collection: %v", err)
	}
}

// TestGitPointerWithoutMappingPreservesOrdinaryReview verifies that an
// optional .gitmodules file cannot hide parent pointers or ordinary files.
func TestGitPointerWithoutMappingPreservesOrdinaryReview(t *testing.T) {
	fixture := newGitFixture(t)
	gitCommand(t, fixture.directory, "rm", ".gitmodules")
	gitCommand(
		t,
		fixture.directory,
		"commit",
		"-m",
		"Keep an embedded repository pointer without a mapping",
	)
	fixture.head = domain.HeadSHA(gitCommand(t, fixture.directory, "rev-parse", "HEAD"))
	ctx, pullRequest, ref := openGitFixtureReview(t, fixture)

	pointer, err := fixture.client.GetFileContent(
		ctx,
		ref.InstallationID,
		ref.Repository,
		"lib/module",
		ref.Head,
	)
	if err != nil || pointer.Metadata != "" ||
		string(pointer.Data) != "Subproject commit "+string(fixture.root)+"\n" {
		t.Fatalf("unmapped parent pointer = %+v: %v", pointer, err)
	}

	source, err := fixture.client.GetFileContent(
		ctx,
		ref.InstallationID,
		ref.Repository,
		gitFixtureSourcePath,
		ref.Head,
	)
	if err != nil || string(source.Data) != gitFixtureSourceContent {
		t.Fatalf("ordinary source = %q: %v", source.Data, err)
	}

	input, err := diff.NewCollector(fixture.client).Collect(ctx, ref, pullRequest)
	if err != nil {
		t.Fatal(err)
	}

	found := false

	for _, file := range input.Files {
		if file.Path == "lib/module" {
			_, anchored := file.ChangedRightLines[1]
			if !anchored || !file.CoverageComplete || file.CurrentContent != string(pointer.Data) {
				t.Fatalf("unmapped pointer review = %+v", file)
			}

			found = true
		}
	}

	if !found {
		t.Fatal("The collector omitted the parent repository pointer")
	}

	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	if fixture.contentRequests != 0 {
		t.Fatalf("unmapped pointer content API requests=%d", fixture.contentRequests)
	}
}

// TestGitMalformedMappingStillFails verifies that an existing invalid mapping
// remains an explicit source failure.
func TestGitMalformedMappingStillFails(t *testing.T) {
	fixture := newGitFixture(t)
	writeGitFixtureFile(t, fixture.directory, ".gitmodules", "[unterminated\n")
	gitCommand(t, fixture.directory, "add", ".gitmodules")
	gitCommand(t, fixture.directory, "commit", "-m", "Create an invalid submodule mapping fixture")
	head := domain.HeadSHA(gitCommand(t, fixture.directory, "rev-parse", "HEAD"))

	_, err := fixture.client.GetFileContent(
		t.Context(),
		77,
		fixture.repo,
		gitFixtureSourcePath,
		head,
	)
	if err == nil {
		t.Fatal("An existing malformed mapping did not fail source acquisition")
	}
}

package review_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/testgit"
)

const (
	gitFixtureUserKey            = "user"
	gitFixtureLoginKey           = "login"
	gitFixtureDeliveryID         = "delivery-1"
	inspectionGitNetworkPath     = "network.tf"
	inspectionGitActivationPath  = "network/activate.sh"
	inspectionGitMissingScenario = "missing"
)

func (fixture *serviceFixture) job() domain.ReviewJob {
	head := fixture.head
	if head == "" {
		head = domain.HeadSHA(testHeadSHA)
	}

	var job domain.ReviewJob

	job.DeliveryID = gitFixtureDeliveryID
	job.PullRequestRef = domain.PullRequestRef{
		Repository: domain.Repository{Owner: "owner", Name: "repo"},
		Number:     testPRNumber, InstallationID: 99, Head: head,
	}

	return job
}

func (fixture *serviceFixture) collectionBase() string {
	if fixture.gitBase != "" {
		return fixture.gitBase
	}

	return coveragePriorHead
}

// forcedJob creates a fresh review job for a force-review label.
func (fixture *serviceFixture) forcedJob() domain.ReviewJob {
	job := fixture.job()
	job.DeliveryID = "delivery-forced"
	job.Forced = true

	return job
}

func contentCollectionRepository(t *testing.T, options []contentCollectionOptions) (string, string, string, string) {
	t.Helper()
	directory := testgit.New(t)
	modules := "[submodule \"zinit\"]\npath = " + collectionSubmodulePath + "\nurl = " + collectionSubmoduleURL + "\n"
	files := map[string]string{collectionFixtureMainPath: "package main\n", ".gitmodules": modules}
	pointers := map[string]string{collectionSubmodulePath: collectionSubmoduleOld}

	title := collectionFixtureTitleKey

	if len(options) > 0 && options[0].onlyMetadata {
		files = nil
		pointers = nil
		title = ""
	}

	base := testgit.Commit(t, directory, "Create collection base", files, pointers)
	if files != nil {
		files[collectionFixtureMainPath] = "package main\nadded\n"
		pointers[collectionSubmodulePath] = collectionSubmoduleNew
	}

	head := testgit.Commit(t, directory, "Change collection sources", files, pointers)

	return directory, base, head, title
}

func inspectionServiceRepository(t *testing.T, scenario string) (string, string, string) {
	t.Helper()
	directory := testgit.New(t)
	before := "resource \"terraform_data\" \"network\" {\n" +
		"  input = {\n    bridge = \"vmbr1\"\n  }\n" +
		"  provisioner \"remote-exec\" {\n" +
		"    script = \"${path.module}/network/activate.sh\"\n  }\n}\n"

	files := map[string]string{inspectionGitNetworkPath: before, inspectionGitActivationPath: inspectionActivationSource}

	if scenario == inspectionGitMissingScenario {
		delete(files, inspectionGitActivationPath)
	}

	base := testgit.Commit(t, directory, "Create host network source", files, nil)
	files[inspectionGitNetworkPath] = strings.Replace(before, "vmbr1", "uplink", 1)
	head := testgit.Commit(t, directory, "Rename the upstream bridge", files, nil)
	workingPath := filepath.Join(directory, inspectionGitActivationPath)

	directoryError := os.MkdirAll(filepath.Dir(workingPath), 0o700)
	if directoryError != nil {
		t.Fatal(directoryError)
	}

	writeError := os.WriteFile(workingPath, []byte("uncommitted activation source\n"), 0o600)
	if writeError != nil {
		t.Fatal(writeError)
	}

	return directory, base, head
}

func (fixture *serviceFixture) gitFindingSummary() review.Summary {
	summary := findingDetailsSummary()
	summary.Head = fixture.job().Head

	return summary
}

func newCommitCollectionFixture(t *testing.T, model *sequenceModel, options contentCollectionOptions) *serviceFixture {
	t.Helper()
	fixture, _ := newContentCollectionFixture(t, model, options)

	for index := range options.commits {
		if options.commits[index].SHA == domain.HeadSHA(testHeadSHA) {
			options.commits[index].SHA = fixture.job().Head
		}
	}

	for index := range model.results[0].Findings {
		if model.results[0].Findings[index].CommitSHA == domain.HeadSHA(testHeadSHA) {
			model.results[0].Findings[index].CommitSHA = fixture.job().Head
		}
	}

	return fixture
}

func (fixture *serviceFixture) seedDismissedGitReview(head domain.HeadSHA) {
	fixture.state.reviewPages = [][]map[string]any{{{
		"id": float64(4100), collectionFixtureCommitIDKey: string(head),
		collectionFixtureStateKey: collectionFixtureDismissedState,
		collectionFixtureBodyKey:  marker.Review(head, domain.ReviewDecisionRequestChanges),
		gitFixtureUserKey:         map[string]any{gitFixtureLoginKey: testBotLogin},
	}}}
}

func assertGitReviewProjection(t *testing.T, native map[string]any, head domain.HeadSHA, finding domain.Finding) {
	t.Helper()

	body, ok := native[collectionFixtureBodyKey].(string)
	if !ok ||
		native[collectionFixtureCommitIDKey] != string(head) ||
		native[collectionFixtureStateKey] != collectionFixtureRejectionState ||
		strings.Count(body,
			finding.Body) != 1 || strings.Count(body,
		finding.Suggestion) != 1 {
		t.Fatalf("The current-head rejection must contain the complete current metadata finding and correction: %v.", native)
	}

	markerHead, found := marker.FindReview(body)
	if !found || markerHead != head {
		t.Fatal("The new rejection must retain a checkpoint for the current head.")
	}
}

package review_test

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/review"
)

const (
	collectionSubmodulePath = "lib/zinit"
	collectionSubmoduleOld  = "db9e267184c85a26056c2646222f48df609cecd5"
	collectionSubmoduleNew  = "aa243da8c5ba1e1781f35dd98e8515bd82dd82c4"
	collectionSubmoduleURL  = "https://github.com/zdharma-continuum/zinit.git"
)

// Extend the existing GitHub fixture with real content collection endpoints.
// Review publication still uses the same mutable HTTP state as every service run.
func newContentCollectionFixture(t *testing.T, model *sequenceModel) (*serviceFixture, *atomic.Int32) {
	t.Helper()
	fixture := newServiceFixture(t, serviceFixtureOptions{model: model})
	contentStatus := &atomic.Int32{}
	contentStatus.Store(http.StatusOK)
	files := []map[string]any{
		{"filename": "main.go", "status": "modified", "patch": "@@ -1 +1,2 @@\n package main\n+added\n"},
		{"filename": collectionSubmodulePath, "status": "modified", "patch": "@@ -1 +1 @@\n-Subproject commit " + collectionSubmoduleOld + "\n+Subproject commit " + collectionSubmoduleNew + "\n"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			switch {
			case strings.HasSuffix(request.URL.Path, "/files"):
				serviceWriteJSON(writer, http.StatusOK, files)
				return
			case strings.Contains(request.URL.Path, "/compare/"):
				serviceWriteJSON(writer, http.StatusOK, map[string]any{"status": "ahead", "files": files, "merge_base_commit": map[string]any{"sha": coveragePriorHead}})
				return
			case strings.Contains(request.URL.Path, "/contents/"):
				if contentStatus.Load() != http.StatusOK {
					http.Error(writer, "content service unavailable", int(contentStatus.Load()))
					return
				}
				if strings.HasSuffix(request.URL.Path, "/"+collectionSubmodulePath) {
					serviceWriteJSON(writer, http.StatusOK, map[string]any{"type": "submodule", "path": collectionSubmodulePath, "sha": collectionSubmoduleNew, "submodule_git_url": collectionSubmoduleURL})
					return
				}
				serviceWriteJSON(writer, http.StatusOK, map[string]any{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("package main\nadded\n"))})
				return
			}
		}
		fixture.state.mu.Lock()
		defer fixture.state.mu.Unlock()
		handleServiceRequest(writer, request, fixture.state)
	}))
	t.Cleanup(server.Close)
	apiURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	graphqlURL, err := url.Parse(server.URL + "/graphql")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := githubapp.NewClient(config.Config{
		GitHubAppID: testGitHubAppID, GitHubPrivateKey: serviceTestPrivateKey(t),
		GitHubBotLogin: testBotLogin, GitHubAPIBaseURL: apiURL, GitHubGraphQLURL: graphqlURL,
	}, server.Client(), func() time.Time { return time.Unix(1_700_000_000, 0) }, logger)
	fixture.service = review.NewService(client, diff.NewCollector(client), model, fixture.reconciler,
		queue.NewKeyedLocker(), testBotLogin, testMinimumImportance,
		config.DefaultReviewMaxFiles, config.DefaultReviewMaxChunks,
		config.DefaultReviewChunkTimeout, testClock(8*time.Second), logger)
	return fixture, contentStatus
}

func TestContentCollectionSubmoduleCleanRunApproves(t *testing.T) {
	model := &sequenceModel{results: []domain.ReviewResult{{}}}
	fixture, _ := newContentCollectionFixture(t, model)
	seedReviewedBaseline(fixture)
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertContentCollectionApproved(t, fixture)
	if len(model.prompts) != 1 {
		t.Fatalf("model prompts = %d, want one complete chunk", len(model.prompts))
	}
	for _, evidence := range []string{collectionSubmoduleOld, collectionSubmoduleNew, collectionSubmoduleURL, "main.go"} {
		if !strings.Contains(model.prompts[0], evidence) {
			t.Errorf("model input omitted %q", evidence)
		}
	}
}

func TestContentCollectionFailurePreservesReviewAndRetryRecovers(t *testing.T) {
	model := &sequenceModel{results: []domain.ReviewResult{{}}}
	fixture, contentStatus := newContentCollectionFixture(t, model)
	prior := marker.State{LastReviewed: domain.HeadSHA(coveragePriorHead), RunID: "delivery-0", Status: marker.StateDone, Pending: []string{"pending-chunk"}, Completed: []string{"completed-chunk"}}
	fixture.state.issueComments = []map[string]any{{"id": float64(2000), "body": "## Review\n" + marker.EncodeState(prior), "user": map[string]any{"login": testBotLogin}}}
	fixture.state.reviewPages = [][]map[string]any{{{"id": float64(4100), "commit_id": coveragePriorHead, "state": "CHANGES_REQUESTED", "body": "Changes requested.\n" + marker.Review(domain.HeadSHA(coveragePriorHead), domain.ReviewDecisionRequestChanges), "user": map[string]any{"login": testBotLogin}}}}
	contentStatus.Store(http.StatusBadGateway)
	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run succeeded after GitHub content fetch returned 502")
	}
	if fixture.state.lastSubmitReview != nil || fixture.state.lastUpdateReview != nil || len(fixture.state.dismissals) != 0 {
		t.Fatalf("failed read mutated review: submit=%v update=%v dismissals=%v", fixture.state.lastSubmitReview, fixture.state.lastUpdateReview, fixture.state.dismissals)
	}
	state := decodedSummaryState(t, fixture)
	if state.LastReviewed != prior.LastReviewed || !reflect.DeepEqual(state.Pending, prior.Pending) || !reflect.DeepEqual(state.Completed, prior.Completed) {
		t.Fatalf("failed read changed checkpoint: got %+v, prior %+v", state, prior)
	}
	if state.Status != marker.StateFailed || fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf("failed read was not reported: state=%s check=%v", state.Status, fixture.state.lastUpdateCheckRun)
	}
	if len(model.prompts) != 0 {
		t.Fatalf("model calls = %d after content fetch failure, want zero", len(model.prompts))
	}
	contentStatus.Store(http.StatusOK)
	job := fixture.job()
	job.DeliveryID = "delivery-content-recovered"
	if err := fixture.run(context.Background(), job); err != nil {
		t.Fatalf("recovered Run: %v", err)
	}
	assertContentCollectionApproved(t, fixture)
	if len(fixture.state.dismissals) != 1 {
		t.Fatalf("dismissals = %v, want stale requested-changes review dismissed", fixture.state.dismissals)
	}
}

func TestContentCollectionSubmoduleFindingRequestsChanges(t *testing.T) {
	model := &sequenceModel{results: []domain.ReviewResult{{Findings: []domain.Finding{{
		Path: collectionSubmodulePath, StartLine: 1, EndLine: 1, Title: "Pinned revision removes required initialization", Body: "The selected revision removes the initialization required by the caller.",
		Evidence: "Subproject commit " + collectionSubmoduleNew, Importance: testMinimumImportance,
	}}}}}
	fixture, _ := newContentCollectionFixture(t, model)
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) || len(fixture.state.streamedComments) != 1 {
		t.Fatalf("substantive finding lost: review=%v comments=%v", fixture.state.lastSubmitReview, fixture.state.streamedComments)
	}
	if !strings.Contains(failureSummaryComment(t, fixture), "| Coverage complete | yes |") {
		t.Fatal("submodule finding falsely reports incomplete coverage")
	}
}

func assertContentCollectionApproved(t *testing.T, fixture *serviceFixture) {
	t.Helper()
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("review event = %v, want APPROVE", fixture.state.lastSubmitReview["event"])
	}
	if !strings.Contains(failureSummaryComment(t, fixture), "| Coverage complete | yes |") {
		t.Fatal("successful content collection reports incomplete coverage")
	}
	state := decodedSummaryState(t, fixture)
	if state.LastReviewed != domain.HeadSHA(testHeadSHA) || len(state.Pending) != 0 {
		t.Fatalf("completed run checkpoint = %+v, want reviewed head and no pending work", state)
	}
}

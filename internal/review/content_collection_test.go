package review_test

import (
	"context"
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
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/testgit"
)

const (
	collectionFixtureMainPath       = "main.go"
	collectionFixtureNumberKey      = "number"
	collectionFixtureStateKey       = "state"
	collectionFixtureOpenState      = "open"
	collectionFixtureMergedKey      = "merged"
	collectionFixtureDraftKey       = "draft"
	collectionFixtureTitleKey       = "title"
	collectionFixtureBodyKey        = "body"
	collectionFixtureHeadKey        = "head"
	collectionFixtureSHAKey         = "sha"
	collectionFixtureBaseKey        = "base"
	collectionFixtureMessageKey     = "message"
	collectionFixtureSearchPath     = "/search/code"
	collectionFixturePriorDelivery  = "delivery-0"
	collectionFixtureCommitIDKey    = "commit_id"
	collectionFixtureRejectionState = "CHANGES_REQUESTED"
	collectionFixtureDismissedState = "DISMISSED"
)

const (
	collectionSubmodulePath = "lib/zinit"
	collectionSubmoduleOld  = "db9e267184c85a26056c2646222f48df609cecd5"
	collectionSubmoduleNew  = "aa243da8c5ba1e1781f35dd98e8515bd82dd82c4"
	collectionSubmoduleURL  = "https://github.com/zdharma-continuum/zinit.git"
)

// Extend the existing GitHub fixture with real content collection endpoints.
// Review publication still uses the same mutable HTTP state as every service run.
type contentCollectionOptions struct {
	description           string
	onlyMetadata          bool
	currentDescription    func() string
	commits               []githubapp.PullRequestCommit
	advertisedCommitCount *int
}

func newContentCollectionFixture(
	t *testing.T,
	model review.Model,
	options ...contentCollectionOptions,
) (*serviceFixture, *atomic.Int32) {
	t.Helper()
	fixture := newServiceFixture(t, serviceFixtureOptions{model: model})
	contentStatus := &atomic.Int32{}
	contentStatus.Store(http.StatusOK)

	directory, base, head, title := contentCollectionRepository(t, options)

	fixture.head, fixture.gitBase, fixture.state.headSHA = domain.HeadSHA(head), base, head
	gitHandler := testgit.Handler(t, directory)
	server := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if strings.Contains(request.URL.Path, ".git/") {
				if contentStatus.Load() != http.StatusOK {
					http.Error(writer, "Git service unavailable", int(contentStatus.Load()))
					return
				}

				gitHandler.ServeHTTP(writer, request)

				return
			}

			if request.Method == http.MethodGet && isPullRequestRead(request) {
				description, commitCount := contentCollectionMetadata(options)

				fixture.state.mu.Lock()
				currentHead := fixture.state.headSHA
				fixture.state.mu.Unlock()
				serviceWriteJSON(
					writer,
					http.StatusOK,
					map[string]any{
						collectionFixtureNumberKey: testPRNumber,
						collectionFixtureStateKey:  collectionFixtureOpenState,
						collectionFixtureMergedKey: false,
						collectionFixtureDraftKey:  false,
						collectionFixtureTitleKey:  title,
						collectionFixtureBodyKey:   description,
						"commits":                  commitCount,
						collectionFixtureHeadKey: map[string]string{
							collectionFixtureSHAKey: currentHead,
						},
						collectionFixtureBaseKey: map[string]string{collectionFixtureSHAKey: base},
					},
				)

				return
			}

			if request.Method == http.MethodGet && len(options) > 0 &&
				strings.HasSuffix(request.URL.Path, "/commits") {
				commits := make([]map[string]any, 0, len(options[0].commits))
				for _, commit := range options[0].commits {
					commits = append(
						commits,
						map[string]any{
							collectionFixtureSHAKey: commit.SHA,
							"commit": map[string]any{
								collectionFixtureMessageKey: commit.Message,
							},
						},
					)
				}

				serviceWriteJSON(writer, http.StatusOK, commits)

				return
			}

			if contentCollectionAPIRequest(request) {
				t.Errorf("Unexpected repository-content API request: %s", request.URL.Path)
				http.Error(writer, "Repository-content APIs are forbidden", http.StatusForbidden)

				return
			}

			fixture.state.mu.Lock()
			defer fixture.state.mu.Unlock()
			handleServiceRequest(writer, request, fixture.state)
		}),
	)
	t.Cleanup(server.Close)
	apiURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	graphqlURL, err := url.Parse(server.URL + "/graphql")
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.DiscardHandler)

	var githubConfig config.Config

	githubConfig.GitHubAppID = testGitHubAppID
	examplePrivateKey := serviceTestPrivateKey(t)
	githubConfig.GitHubPrivateKey = examplePrivateKey
	githubConfig.GitHubBotLogin = testBotLogin
	githubConfig.GitHubAPIBaseURL = apiURL
	githubConfig.GitHubGraphQLURL = graphqlURL
	client := githubapp.NewClient(
		githubConfig,
		testgit.Client(t, server),
		func() time.Time {
			return time.Unix(1_700_000_000, 0)
		},
		logger,
	)
	t.Cleanup(func() { _ = client.Close() })
	fixture.github = client
	fixture.service = review.NewService(
		client,
		diff.NewCollector(client),
		model,
		fixture.reconciler,
		queue.NewKeyedLocker(),
		testBotLogin,
		testMinimumImportance,
		config.DefaultReviewMaxFiles,
		config.DefaultReviewMaxChunks,
		config.DefaultReviewChunkTimeout,
		nil,
		testClock(8*time.Second),
		logger,
		config.MaximumChunkConcurrency,
		config.MaximumPromptBytes,
		policytest.Load(t),
	)

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

	for _, evidence := range []string{
		collectionSubmoduleOld,
		collectionSubmoduleNew,
		collectionSubmoduleURL,
		"main.go",
	} {
		if !strings.Contains(model.prompts[0], evidence) {
			t.Errorf("model input omitted %q", evidence)
		}
	}
}

func TestContentCollectionFailureDismissesRejectionAndRetryRecovers(t *testing.T) {
	model := &sequenceModel{results: []domain.ReviewResult{{}}}
	fixture, contentStatus := newContentCollectionFixture(t, model)
	prior := marker.State{
		PolicyRevision: "",
		ForcedBy:       "",
		Unread:         nil,
		LastReviewed:   domain.HeadSHA(fixture.collectionBase()),
		RunID:          collectionFixturePriorDelivery,
		Status:         marker.StateDone,
		Pending:        []string{"pending-chunk"},
		Completed:      []string{"completed-chunk"},
	}
	fixture.state.issueComments = []map[string]any{
		{
			"id":   float64(2000),
			"body": "## Review\n" + marker.EncodeState(prior),
			"user": map[string]any{"login": testBotLogin},
		},
	}
	fixture.state.reviewPages = [][]map[string]any{
		{
			{
				"id":                         float64(4100),
				collectionFixtureCommitIDKey: fixture.collectionBase(),
				collectionFixtureStateKey:    collectionFixtureRejectionState,
				collectionFixtureBodyKey: "Changes requested.\n" + marker.Review(
					domain.HeadSHA(fixture.collectionBase()),
					domain.ReviewDecisionRequestChanges,
				),
				"user": map[string]any{"login": testBotLogin},
			},
		},
	}
	contentStatus.Store(http.StatusBadGateway)
	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run succeeded after GitHub content fetch returned 502")
	}

	if fixture.state.lastSubmitReview != nil || fixture.state.lastUpdateReview != nil ||
		len(fixture.state.dismissals) != 1 ||
		fixture.state.dismissals[0]["review_id"] != "4100" {
		t.Fatalf(
			"failed read did not withdraw only the previous rejection: submit=%v update=%v dismissals=%v",
			fixture.state.lastSubmitReview,
			fixture.state.lastUpdateReview,
			fixture.state.dismissals,
		)
	}

	state := decodedSummaryState(t, fixture)
	if state.LastReviewed != prior.LastReviewed ||
		!reflect.DeepEqual(state.Pending, prior.Pending) ||
		!reflect.DeepEqual(state.Completed, prior.Completed) {
		t.Fatalf("failed read changed checkpoint: got %+v, prior %+v", state, prior)
	}

	if state.Status != marker.StateFailed ||
		fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf(
			"failed read was not reported: state=%s check=%v",
			state.Status,
			fixture.state.lastUpdateCheckRun,
		)
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

	if fixture.state.lastSubmitReview[collectionFixtureCommitIDKey] != string(fixture.job().Head) ||
		len(fixture.state.submittedReviews) != 1 {
		t.Fatalf(
			"recovered review = %v, want one approval on current head",
			fixture.state.submittedReviews,
		)
	}

	if len(fixture.state.issueComments) != 1 || fixture.state.issueCommentUpdates == 0 {
		t.Fatalf(
			"summary comments = %d, updates = %d, want original summary updated",
			len(fixture.state.issueComments),
			fixture.state.issueCommentUpdates,
		)
	}
}

func TestContentCollectionSubmoduleFindingRequestsChanges(t *testing.T) {
	model := &sequenceModel{
		results: []domain.ReviewResult{
			{
				Findings: []domain.Finding{
					{
						Path:       collectionSubmodulePath,
						StartLine:  1,
						EndLine:    1,
						Title:      "Pinned revision removes required initialization",
						Body:       "The selected revision removes the initialization required by the caller.",
						Evidence:   "Subproject commit " + collectionSubmoduleNew,
						Importance: testMinimumImportance,
					},
				},
			},
		},
	}
	fixture, _ := newContentCollectionFixture(t, model)
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) ||
		len(fixture.state.streamedComments) != 1 {
		t.Fatalf(
			"substantive finding lost: review=%v comments=%v",
			fixture.state.lastSubmitReview,
			fixture.state.streamedComments,
		)
	}

	if !strings.Contains(
		failureSummaryComment(t, fixture),
		"| All file content inspected | yes |",
	) {
		t.Fatal("submodule finding falsely reports incomplete coverage")
	}
}

func TestContentCollectionForcedRunRecoversIncorrectlyReviewedHead(t *testing.T) {
	model := &sequenceModel{results: []domain.ReviewResult{{}}}
	fixture, _ := newContentCollectionFixture(t, model)
	seedStateNaming(fixture, string(fixture.job().Head))
	fixture.state.reviewPages = [][]map[string]any{
		{
			{
				"id":                         float64(4100),
				collectionFixtureCommitIDKey: string(fixture.job().Head),
				collectionFixtureStateKey:    collectionFixtureRejectionState,

				collectionFixtureBodyKey: "Changes requested.\n" + testUnreviewedHeadReason + "\n" +
					marker.Review(fixture.job().Head, domain.ReviewDecisionRequestChanges),

				"user": map[string]any{"login": testBotLogin},
			},
		},
	}
	if err := fixture.run(context.Background(), fixture.forcedJob()); err != nil {
		t.Fatalf("forced Run: %v", err)
	}

	assertContentCollectionApproved(t, fixture)

	if len(model.prompts) != 1 || !strings.Contains(model.prompts[0], collectionSubmoduleNew) ||
		!strings.Contains(model.prompts[0], collectionSubmoduleURL) {
		t.Fatalf("forced run did not analyze current submodule context: prompts=%v", model.prompts)
	}

	if fixture.state.lastSubmitReview[collectionFixtureCommitIDKey] != string(fixture.job().Head) ||
		len(fixture.state.submittedReviews) != 1 {
		t.Fatalf(
			"forced review = %v, want one approval on the current head",
			fixture.state.submittedReviews,
		)
	}

	if len(fixture.state.issueComments) != 1 ||
		fixture.state.issueComments[0]["id"] != float64(2000) ||
		fixture.state.issueCommentUpdates == 0 {
		t.Fatalf("forced run replaced original summary: comments=%v", fixture.state.issueComments)
	}

	if len(fixture.state.streamedComments) != 0 {
		t.Fatalf("forced clean run posted findings: %v", fixture.state.streamedComments)
	}
}

func assertContentCollectionApproved(t *testing.T, fixture *serviceFixture) {
	t.Helper()
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("review event = %v, want APPROVE", fixture.state.lastSubmitReview["event"])
	}

	if !strings.Contains(
		failureSummaryComment(t, fixture),
		"| All file content inspected | yes |",
	) {
		t.Fatal("successful content collection reports incomplete coverage")
	}

	state := decodedSummaryState(t, fixture)
	if state.LastReviewed != domain.HeadSHA(string(fixture.job().Head)) || len(state.Pending) != 0 {
		t.Fatalf("completed run checkpoint = %+v, want reviewed head and no pending work", state)
	}
}

func contentCollectionAPIRequest(request *http.Request) bool {
	endpoints := []string{
		"/contents/",
		"/git/trees/",
		"/git/blobs/",
		"/compare/",
		"/tarball/",
		collectionFixtureSearchPath,
	}
	for _, endpoint := range endpoints {
		if strings.Contains(request.URL.Path, endpoint) {
			return true
		}
	}

	return strings.HasSuffix(request.URL.Path, "/files")
}

func contentCollectionMetadata(options []contentCollectionOptions) (string, int) {
	if len(options) == 0 {
		return "", 0
	}

	option := options[0]

	description := option.description
	if option.currentDescription != nil {
		description = option.currentDescription()
	}

	commitCount := len(option.commits)
	if option.advertisedCommitCount != nil {
		commitCount = *option.advertisedCommitCount
	}

	return description, commitCount
}

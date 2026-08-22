package githubapp_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

const (
	testHeadSHA  = "a3c4f1cac7f595bc824704b9d2a1f1191630dc32"
	testBaseSHA  = "b4d5e2dbd8f606cd935815c0e3b2f2202741ed43"
	testBotLogin = "test-review-agent[bot]"
)

func TestAppJWTContainsExpectedClaimsAndValidSignature(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server := newTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	var capturedJWT string
	server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/access_tokens") {
			capturedJWT = strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
			writeJSON(writer, http.StatusCreated, map[string]any{
				"token":      "ghs_installation",
				"expires_at": time.Unix(1_700_000_600, 0).UTC().Format(time.RFC3339),
			})
			return
		}
		if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/") {
			writeJSON(writer, http.StatusOK, map[string]any{
				"number": float64(1),
				"draft":  false,
				"title":  "title",
				"body":   "body",
				"head":   map[string]any{"sha": testHeadSHA},
				"base":   map[string]any{"sha": testBaseSHA},
			})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	})

	_, err := client.GetPullRequest(context.Background(), 99, testRepo(), 1)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if capturedJWT == "" {
		t.Fatal("installation token request missing jwt")
	}

	claims, err := parseJWTClaimsForTest(capturedJWT)
	if err != nil {
		t.Fatalf("parseJWTClaimsForTest: %v", err)
	}
	if claims.Issuer != "12345" {
		t.Fatalf("iss = %q, want 12345", claims.Issuer)
	}
	if claims.IssuedAt != 1_699_999_940 {
		t.Fatalf("iat = %d, want %d", claims.IssuedAt, 1_699_999_940)
	}
	if claims.ExpiresAt != 1_700_000_540 {
		t.Fatalf("exp = %d, want %d", claims.ExpiresAt, 1_700_000_540)
	}
	if err := verifyJWTSignatureForTest(capturedJWT, &privateKey.PublicKey); err != nil {
		t.Fatalf("verifyJWTSignatureForTest: %v", err)
	}
}

func TestInstallationTokenIsCachedAndRefreshedBeforeExpiry(t *testing.T) {
	privateKey := testPrivateKey(t)
	now := time.Unix(1_700_000_000, 0)
	client, server, state := newStatefulTestClient(t, privateKey, now)
	defer server.Close()

	state.installationTokenCount = 0
	state.installationExpiresAt = now.Add(1 * time.Hour).UTC().Format(time.RFC3339)

	_, err := client.GetPullRequest(context.Background(), 99, testRepo(), 1)
	if err != nil {
		t.Fatalf("first GetPullRequest: %v", err)
	}
	_, err = client.GetPullRequest(context.Background(), 99, testRepo(), 1)
	if err != nil {
		t.Fatalf("second GetPullRequest: %v", err)
	}
	if state.installationTokenCount != 1 {
		t.Fatalf("installation token requests = %d, want 1", state.installationTokenCount)
	}

	now = now.Add(56 * time.Minute)
	state.nowValue = now
	_, err = client.GetPullRequest(context.Background(), 99, testRepo(), 1)
	if err != nil {
		t.Fatalf("third GetPullRequest: %v", err)
	}
	if state.installationTokenCount != 2 {
		t.Fatalf("installation token requests after refresh = %d, want 2", state.installationTokenCount)
	}
}

func TestClientErrorsExposeStatusWithoutCredentials(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server := newTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	secretToken := "ghs_supersecret_token_value"
	server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/access_tokens") {
			writeJSON(writer, http.StatusCreated, map[string]any{
				"token":      secretToken,
				"expires_at": time.Unix(1_700_000_600, 0).UTC().Format(time.RFC3339),
			})
			return
		}
		if strings.Contains(request.URL.Path, "/pulls/1") {
			writeJSON(writer, http.StatusUnauthorized, map[string]any{
				"message": fmt.Sprintf("bad auth Bearer %s", secretToken),
			})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	})

	_, err := client.GetPullRequest(context.Background(), 99, testRepo(), 1)
	if err == nil {
		t.Fatal("GetPullRequest: want error")
	}
	var apiErr githubapp.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", apiErr.StatusCode)
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("error leaks token: %q", err.Error())
	}
}

func TestRESTPaginationLoadsEveryPage(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.reviewPages = [][]map[string]any{
		{
			{"id": float64(1), "commit_id": testHeadSHA, "state": "COMMENTED", "body": "one", "user": map[string]any{"login": "bot"}},
		},
		{
			{"id": float64(2), "commit_id": testHeadSHA, "state": "COMMENTED", "body": "two", "user": map[string]any{"login": "bot"}},
		},
	}

	reviews, err := client.ListReviews(context.Background(), 99, testRepo(), 7)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("review count = %d, want 2", len(reviews))
	}
	if reviews[0].ID != 1 || reviews[1].ID != 2 {
		t.Fatalf("reviews = %+v, want ids 1 and 2", reviews)
	}
}

func TestListChangedFilesPreservesMissingPatch(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.changedFilePages = [][]map[string]any{
		{
			{"filename": "with.patch", "status": "modified", "patch": "@@"},
			{"filename": "without.patch", "status": "added"},
		},
	}

	files, err := client.ListChangedFiles(context.Background(), 99, testRepo(), 3)
	if err != nil {
		t.Fatalf("ListChangedFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("file count = %d, want 2", len(files))
	}
	if !files[0].PatchPresent || files[0].Patch != "@@" {
		t.Fatalf("patched file = %+v", files[0])
	}
	if files[1].PatchPresent || files[1].Patch != "" {
		t.Fatalf("missing patch file = %+v", files[1])
	}
}

func TestGetFileDecodesBase64Content(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.fileContent = map[string]any{
		"content":  "Zm9v\n",
		"encoding": "base64",
	}

	content, err := client.GetFile(
		context.Background(),
		99,
		testRepo(),
		"dir/file.go",
		domain.HeadSHA(testHeadSHA),
	)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if string(content) != "foo" {
		t.Fatalf("content = %q, want foo", string(content))
	}
}

func TestSubmitReviewSendsOneCompletePayload(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	request := githubapp.SubmitReviewRequest{
		CommitID: domain.HeadSHA(testHeadSHA),
		Body:     "summary",
		Event:    domain.ReviewDecisionRequestChanges,
		Comments: []githubapp.InlineComment{
			{
				Path: "main.go",
				Body: "issue",
				Line: 10,
				Side: "RIGHT",
			},
		},
	}
	_, err := client.SubmitReview(context.Background(), 99, testRepo(), 4, request)
	if err != nil {
		t.Fatalf("SubmitReview: %v", err)
	}
	if state.lastSubmitReview == nil {
		t.Fatal("submit review payload missing")
	}
	if state.lastSubmitReview["commit_id"] != testHeadSHA {
		t.Fatalf("commit_id = %v", state.lastSubmitReview["commit_id"])
	}
	if state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v", state.lastSubmitReview["event"])
	}
	comments, ok := state.lastSubmitReview["comments"].([]any)
	if !ok || len(comments) != 1 {
		t.Fatalf("comments = %v", state.lastSubmitReview["comments"])
	}
}

func TestSubmitReviewRejectsUnknownEvent(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	_, err := client.SubmitReview(context.Background(), 99, testRepo(), 4, githubapp.SubmitReviewRequest{
		CommitID: domain.HeadSHA(testHeadSHA),
		Body:     "summary",
		Event:    "SHIP_IT",
	})
	if err == nil {
		t.Fatal("SubmitReview unknown event: want error")
	}
	if state.lastSubmitReview != nil {
		t.Fatal("SubmitReview posted a review for an unknown event")
	}
}

func TestListReviewsPreservesAuthorForMarkerOwnership(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.reviewPages = [][]map[string]any{
		{
			{
				"id":        float64(9),
				"commit_id": testHeadSHA,
				"state":     "CHANGES_REQUESTED",
				"body":      "<!-- marker -->",
				"user":      map[string]any{"login": testBotLogin},
			},
		},
	}

	reviews, err := client.ListReviews(context.Background(), 99, testRepo(), 5)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("review count = %d, want 1", len(reviews))
	}
	if reviews[0].Author != testBotLogin {
		t.Fatalf("author = %q", reviews[0].Author)
	}
}

func TestCheckRunLifecycleUsesExpectedPayloads(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	created, err := client.CreateCheckRun(
		context.Background(),
		99,
		testRepo(),
		domain.HeadSHA(testHeadSHA),
		config.ReviewCheckName,
	)
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	if state.lastCreateCheckRun["status"] != "queued" {
		t.Fatalf("create status = %v", state.lastCreateCheckRun["status"])
	}

	if err := client.StartCheckRun(context.Background(), 99, testRepo(), created.ID, "https://example.test/run"); err != nil {
		t.Fatalf("StartCheckRun: %v", err)
	}
	if state.lastUpdateCheckRun["status"] != "in_progress" {
		t.Fatalf("start status = %v", state.lastUpdateCheckRun["status"])
	}

	if err := client.CompleteCheckRun(context.Background(), 99, testRepo(), created.ID, "success", "done", "finished"); err != nil {
		t.Fatalf("CompleteCheckRun: %v", err)
	}
	if state.lastUpdateCheckRun["status"] != "completed" {
		t.Fatalf("complete status = %v", state.lastUpdateCheckRun["status"])
	}
	if state.lastUpdateCheckRun["conclusion"] != "success" {
		t.Fatalf("conclusion = %v", state.lastUpdateCheckRun["conclusion"])
	}
	output, ok := state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok || output["title"] != "done" || output["summary"] != "finished" {
		t.Fatalf("output = %v, want distinct title and summary", state.lastUpdateCheckRun["output"])
	}
}

func TestDismissReviewSendsTheDismissalPayload(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	err := client.DismissReview(context.Background(), 99, testRepo(), 4, 42, "no longer stands")
	if err != nil {
		t.Fatalf("DismissReview: %v", err)
	}
	if state.lastDismissReview == nil {
		t.Fatal("dismiss review payload missing")
	}
	if state.lastDismissPath != "/repos/owner/repo/pulls/4/reviews/42/dismissals" {
		t.Fatalf("path = %q, want the review dismissals path", state.lastDismissPath)
	}
	if state.lastDismissReview["event"] != "DISMISS" {
		t.Fatalf("event = %v, want DISMISS", state.lastDismissReview["event"])
	}
	if state.lastDismissReview["message"] != "no longer stands" {
		t.Fatalf("message = %v, want the supplied message", state.lastDismissReview["message"])
	}
}

// A reported GitHub failure names the request, the status, and the reason
// GitHub gave. A bare phrase leaves the reader nothing to act on.
func TestDismissReviewFailureNamesTheRequestAndTheReason(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.dismissReviewStatus = http.StatusForbidden
	err := client.DismissReview(context.Background(), 99, testRepo(), 4, 42, "message")
	if err == nil {
		t.Fatal("DismissReview: want error")
	}
	for _, want := range []string{
		"PUT",
		"/repos/owner/repo/pulls/4/reviews/42/dismissals",
		"403",
		"Forbidden",
		"dismiss review failed",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to name %q", err, want)
		}
	}
}

// A request that never reaches GitHub reports the transport failure rather than
// a fixed phrase, and keeps the underlying error matchable.
func TestUnreachableGitHubReportsTheTransportFailure(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, _ := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	server.Close()

	_, err := client.GetPullRequest(context.Background(), 99, testRepo(), 4)
	if err == nil {
		t.Fatal("GetPullRequest: want error")
	}
	if !strings.Contains(err.Error(), "reach GitHub") {
		t.Fatalf("error = %q, want the stage that failed", err)
	}
	if !strings.Contains(err.Error(), "connect") && !strings.Contains(err.Error(), "refused") {
		t.Fatalf("error = %q, want the underlying transport failure", err)
	}
}

func TestEndToEndMoreThanOneHundredReviewThreads(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.threadPages = buildThreadPages(101)

	threads, err := client.ListReviewThreads(context.Background(), 99, testRepo(), 6)
	if err != nil {
		t.Fatalf("ListReviewThreads: %v", err)
	}
	if len(threads) != 101 {
		t.Fatalf("thread count = %d, want 101", len(threads))
	}
}

func TestEndToEndGitHubFailureSetsFailedLifecycle(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.submitReviewStatus = http.StatusInternalServerError
	_, err := client.SubmitReview(context.Background(), 99, testRepo(), 6, githubapp.SubmitReviewRequest{
		CommitID: domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32"),
		Body:     "body",
		Event:    domain.ReviewDecisionApprove,
	})
	if err == nil {
		t.Fatal("SubmitReview: want error")
	}
}

func TestListReviewThreadsPaginatesBeyondOneHundred(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.threadPages = buildThreadPages(150)

	threads, err := client.ListReviewThreads(context.Background(), 99, testRepo(), 6)
	if err != nil {
		t.Fatalf("ListReviewThreads: %v", err)
	}
	if len(threads) != 150 {
		t.Fatalf("thread count = %d, want 150", len(threads))
	}
	if threads[0].NodeID != "thread-0" || threads[149].NodeID != "thread-149" {
		t.Fatalf("thread bounds = %q .. %q", threads[0].NodeID, threads[149].NodeID)
	}
}

func TestListReviewThreadsNormalizesGraphQLBotLogin(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.threadPages = buildThreadPages(1)

	threads, err := client.ListReviewThreads(context.Background(), 99, testRepo(), 6)
	if err != nil {
		t.Fatalf("ListReviewThreads: %v", err)
	}
	if threads[0].RootComment.Author != testBotLogin {
		t.Fatalf("author = %q, want %q", threads[0].RootComment.Author, testBotLogin)
	}
}

func TestListReviewThreadsUsesOriginalLinesForOutdatedComments(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.threadPages = []map[string]any{{
		"repository": map[string]any{
			"pullRequest": map[string]any{
				"reviewThreads": map[string]any{
					"pageInfo": map[string]any{
						"hasNextPage": false,
						"endCursor":   "",
					},
					"nodes": []map[string]any{{
						"id":         "thread-outdated",
						"isResolved": false,
						"isOutdated": true,
						"comments": map[string]any{
							"nodes": []map[string]any{{
								"databaseId":        float64(1),
								"body":              "body",
								"path":              "main.go",
								"line":              nil,
								"startLine":         nil,
								"originalLine":      float64(27),
								"originalStartLine": float64(26),
								"author": map[string]any{
									"__typename": "Bot",
									"login":      strings.TrimSuffix(testBotLogin, "[bot]"),
								},
							}},
						},
					}},
				},
			},
		},
	}}

	threads, err := client.ListReviewThreads(context.Background(), 99, testRepo(), 6)
	if err != nil {
		t.Fatalf("ListReviewThreads: %v", err)
	}
	comment := threads[0].RootComment
	if comment.StartLine != 26 || comment.EndLine != 27 {
		t.Fatalf("lines = %d-%d, want 26-27", comment.StartLine, comment.EndLine)
	}
}

func TestResolveReviewThreadVerifiesMutationResult(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.resolveThreadID = "thread-resolve"
	if err := client.ResolveReviewThread(context.Background(), 99, "thread-resolve"); err != nil {
		t.Fatalf("ResolveReviewThread: %v", err)
	}
	if state.lastResolveThreadID != "thread-resolve" {
		t.Fatalf("resolve thread id = %q", state.lastResolveThreadID)
	}
}

func TestGraphQLErrorsRemainVisible(t *testing.T) {
	privateKey := testPrivateKey(t)
	client, server, state := newStatefulTestClient(t, privateKey, time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.graphQLError = "thread lookup failed"
	_, err := client.ListReviewThreads(context.Background(), 99, testRepo(), 8)
	if err == nil {
		t.Fatal("ListReviewThreads: want error")
	}
	if !strings.Contains(err.Error(), "thread lookup failed") {
		t.Fatalf("error = %q, want graphql message visible", err.Error())
	}
}

type testServerState struct {
	nowValue               time.Time
	installationTokenCount int32
	installationExpiresAt  string
	reviewPages            [][]map[string]any
	reviewPageIndex        int
	changedFilePages       [][]map[string]any
	changedFilePageIndex   int
	fileContent            map[string]any
	lastSubmitReview       map[string]any
	lastCreateCheckRun     map[string]any
	lastUpdateCheckRun     map[string]any
	threadPages            []map[string]any
	threadPageIndex        int
	lastResolveThreadID    string
	resolveThreadID        string
	graphQLError           string
	submitReviewStatus     int
	dismissReviewStatus    int
	lastDismissReview      map[string]any
	lastDismissPath        string
}

func newTestClient(t *testing.T, privateKey *rsa.PrivateKey, now time.Time) (*githubapp.Client, *httptest.Server) {
	client, server, _ := newStatefulTestClient(t, privateKey, now)
	return client, server
}

func newStatefulTestClient(
	t *testing.T,
	privateKey *rsa.PrivateKey,
	now time.Time,
) (*githubapp.Client, *httptest.Server, *testServerState) {
	t.Helper()

	state := &testServerState{
		nowValue:              now,
		installationExpiresAt: now.Add(1 * time.Hour).UTC().Format(time.RFC3339),
		reviewPages:           [][]map[string]any{{}},
		changedFilePages:      [][]map[string]any{{}},
		fileContent: map[string]any{
			"content":  "",
			"encoding": "base64",
		},
		threadPages: []map[string]any{emptyThreadPage(false)},
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if failForbiddenEndpoint(writer, request) {
			return
		}
		handleTestRequest(writer, request, state)
	}))

	apiURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse api URL: %v", err)
	}
	graphqlURL, err := url.Parse(server.URL + "/graphql")
	if err != nil {
		t.Fatalf("Parse graphql URL: %v", err)
	}

	cfg := config.Config{
		Port:             "3000",
		GitHubAppID:      12345,
		GitHubPrivateKey: privateKey, // gitleaks:allow
		GitHubBotLogin:   testBotLogin,
		GitHubAPIBaseURL: apiURL,
		GitHubGraphQLURL: graphqlURL,
	}

	nowFn := func() time.Time {
		return state.nowValue
	}
	client := githubapp.NewClient(cfg, server.Client(), nowFn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return client, server, state
}

func handleTestRequest(writer http.ResponseWriter, request *http.Request, state *testServerState) {
	if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/access_tokens") {
		atomic.AddInt32(&state.installationTokenCount, 1)
		writeJSON(writer, http.StatusCreated, map[string]any{
			"token":      "ghs_installation",
			"expires_at": state.installationExpiresAt,
		})
		return
	}

	if request.Method == http.MethodPost && request.URL.Path == "/graphql" {
		handleGraphQL(writer, request, state)
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/") && strings.HasSuffix(request.URL.Path, "/reviews") {
		writeReviewPage(writer, request, state)
		return
	}

	if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/pulls/") && strings.HasSuffix(request.URL.Path, "/reviews") {
		body, err := readJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		if state.submitReviewStatus != 0 && state.submitReviewStatus != http.StatusOK {
			writeJSON(writer, state.submitReviewStatus, map[string]any{
				"message": "submit review failed",
			})
			return
		}
		state.lastSubmitReview = body
		writeJSON(writer, http.StatusOK, map[string]any{
			"id":        float64(42),
			"commit_id": body["commit_id"],
			"state":     "CHANGES_REQUESTED",
			"body":      body["body"],
			"user":      map[string]any{"login": testBotLogin},
		})
		return
	}

	if request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/dismissals") {
		body, err := readJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		if state.dismissReviewStatus != 0 && state.dismissReviewStatus != http.StatusOK {
			writeJSON(writer, state.dismissReviewStatus, map[string]any{
				"message": "dismiss review failed",
			})
			return
		}
		state.lastDismissReview = body
		state.lastDismissPath = request.URL.Path
		writeJSON(writer, http.StatusOK, map[string]any{
			"id":        float64(42),
			"commit_id": testHeadSHA,
			"state":     "DISMISSED",
			"body":      "",
			"user":      map[string]any{"login": testBotLogin},
		})
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/") && strings.HasSuffix(request.URL.Path, "/files") {
		writeChangedFilePage(writer, request, state)
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/contents/") {
		writeJSON(writer, http.StatusOK, state.fileContent)
		return
	}

	if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/check-runs") {
		body, err := readJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		state.lastCreateCheckRun = body
		writeJSON(writer, http.StatusCreated, map[string]any{
			"id":         float64(77),
			"name":       body["name"],
			"head_sha":   body["head_sha"],
			"status":     body["status"],
			"conclusion": "",
		})
		return
	}

	if request.Method == http.MethodPatch && strings.Contains(request.URL.Path, "/check-runs/") {
		body, err := readJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		state.lastUpdateCheckRun = body
		writeJSON(writer, http.StatusOK, map[string]any{
			"id":         float64(77),
			"name":       config.ReviewCheckName,
			"head_sha":   testHeadSHA,
			"status":     body["status"],
			"conclusion": body["conclusion"],
		})
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/") {
		writeJSON(writer, http.StatusOK, map[string]any{
			"number": float64(1),
			"draft":  false,
			"title":  "title",
			"body":   "body",
			"head":   map[string]any{"sha": testHeadSHA},
			"base":   map[string]any{"sha": testBaseSHA},
		})
		return
	}

	writer.WriteHeader(http.StatusNotFound)
}

func handleGraphQL(writer http.ResponseWriter, request *http.Request, state *testServerState) {
	body, err := readJSONBody(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if state.graphQLError != "" {
		writeJSON(writer, http.StatusOK, map[string]any{
			"errors": []map[string]any{{"message": state.graphQLError}},
		})
		return
	}

	query, _ := body["query"].(string)
	if strings.Contains(query, "resolveReviewThread") {
		variables, _ := body["variables"].(map[string]any)
		threadID, _ := variables["threadID"].(string)
		state.lastResolveThreadID = threadID
		writeJSON(writer, http.StatusOK, map[string]any{
			"data": map[string]any{
				"resolveReviewThread": map[string]any{
					"thread": map[string]any{
						"id":         state.resolveThreadID,
						"isResolved": true,
					},
				},
			},
		})
		return
	}

	page := state.threadPages[state.threadPageIndex]
	state.threadPageIndex++
	writeJSON(writer, http.StatusOK, map[string]any{"data": page})
}

func writeReviewPage(writer http.ResponseWriter, request *http.Request, state *testServerState) {
	if state.reviewPageIndex >= len(state.reviewPages) {
		writeJSON(writer, http.StatusOK, []map[string]any{})
		return
	}
	page := state.reviewPages[state.reviewPageIndex]
	state.reviewPageIndex++
	if state.reviewPageIndex < len(state.reviewPages) {
		next := fmt.Sprintf("http://%s%s", request.Host, request.URL.Path)
		if request.URL.RawQuery != "" {
			next += "?" + request.URL.RawQuery + "&page=2"
		} else {
			next += "?page=2"
		}
		writer.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
	}
	writeJSON(writer, http.StatusOK, page)
}

func writeChangedFilePage(writer http.ResponseWriter, request *http.Request, state *testServerState) {
	if state.changedFilePageIndex >= len(state.changedFilePages) {
		writeJSON(writer, http.StatusOK, []map[string]any{})
		return
	}
	page := state.changedFilePages[state.changedFilePageIndex]
	state.changedFilePageIndex++
	writeJSON(writer, http.StatusOK, page)
}

func buildThreadPages(total int) []map[string]any {
	pages := make([]map[string]any, 0)
	for start := 0; start < total; start += 100 {
		end := start + 100
		if end > total {
			end = total
		}
		nodes := make([]map[string]any, 0, end-start)
		for index := start; index < end; index++ {
			nodes = append(nodes, map[string]any{
				"id":         fmt.Sprintf("thread-%d", index),
				"isResolved": false,
				"comments": map[string]any{
					"nodes": []map[string]any{
						{
							"databaseId": float64(index + 1),
							"body":       "body",
							"path":       "main.go",
							"line":       float64(10),
							"startLine":  float64(10),
							"author": map[string]any{
								"__typename": "Bot",
								"login":      strings.TrimSuffix(testBotLogin, "[bot]"),
							},
						},
					},
				},
			})
		}
		hasNext := end < total
		endCursor := ""
		if hasNext {
			endCursor = fmt.Sprintf("cursor-%d", end)
		}
		pages = append(pages, map[string]any{
			"repository": map[string]any{
				"pullRequest": map[string]any{
					"reviewThreads": map[string]any{
						"pageInfo": map[string]any{
							"hasNextPage": hasNext,
							"endCursor":   endCursor,
						},
						"nodes": nodes,
					},
				},
			},
		})
	}
	return pages
}

func emptyThreadPage(hasNext bool) map[string]any {
	return map[string]any{
		"repository": map[string]any{
			"pullRequest": map[string]any{
				"reviewThreads": map[string]any{
					"pageInfo": map[string]any{
						"hasNextPage": hasNext,
						"endCursor":   "",
					},
					"nodes": []map[string]any{},
				},
			},
		},
	}
}

func failForbiddenEndpoint(writer http.ResponseWriter, request *http.Request) bool {
	if strings.Contains(request.URL.Path, "/issues/comments") {
		http.Error(writer, "issue comments forbidden", http.StatusForbidden)
		return true
	}
	if strings.Contains(request.URL.Path, "/pulls/comments/") && strings.HasSuffix(request.URL.Path, "/replies") {
		http.Error(writer, "replies forbidden", http.StatusForbidden)
		return true
	}
	return false
}

func testRepo() domain.Repository {
	return domain.Repository{Owner: "owner", Name: "repo"}
}

func testPrivateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return privateKey
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func readJSONBody(request *http.Request) (map[string]any, error) {
	defer func() {
		_ = request.Body.Close()
	}()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

type jwtClaims struct {
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Issuer    string `json:"iss"`
}

func parseJWTClaimsForTest(token string) (jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, errors.New("jwt must have three parts")
	}
	payload, err := base64RawURLDecode(parts[1])
	if err != nil {
		return jwtClaims{}, err
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return jwtClaims{}, err
	}
	return claims, nil
}

func verifyJWTSignatureForTest(token string, publicKey *rsa.PublicKey) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("jwt must have three parts")
	}
	signature, err := base64RawURLDecode(parts[2])
	if err != nil {
		return err
	}
	unsigned := parts[0] + "." + parts[1]
	hashed := sha256.Sum256([]byte(unsigned))
	return rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, hashed[:], signature)
}

func base64RawURLDecode(value string) ([]byte, error) {
	pad := strings.Repeat("=", (4-len(value)%4)%4)
	standard := strings.NewReplacer("-", "+", "_", "/").Replace(value + pad)
	return base64.StdEncoding.DecodeString(standard)
}

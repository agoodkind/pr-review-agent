package app_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/app"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
)

const (
	testWebhookSecret = "test-webhook-secret" // gitleaks:allow
	testDefectiveHead = "a3c4f1cac7f595bc824704b9d2a1f1191630dc32"
	testCorrectedHead = "b4d5e2dbd8f606cd935815c0e3b2f2202741ed43"
	testBaseSHA       = "c5e6f3ece9f717de046926d1f4c3f3313852fe54"
	testFindingPath   = "internal/app/handler.go"
	testRepoOwner     = "agoodkind"
	testRepoName      = "pr-review-agent"
	testPRNumber      = 42
	testInstallation  = int64(99)
)

func TestHealthNoExternalCalls(t *testing.T) {
	fixture := newAppFixture(t, appFixtureOptions{})
	defer fixture.close()

	response, err := http.Get(fixture.baseURL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if fixture.githubState.requestCount() != 0 {
		t.Fatalf("github requests = %d, want 0", fixture.githubState.requestCount())
	}
	if fixture.clydeState.requestCount() != 0 {
		t.Fatalf("clyde requests = %d, want 0", fixture.clydeState.requestCount())
	}
}

func TestRootStatusNoExternalCalls(t *testing.T) {
	fixture := newAppFixture(t, appFixtureOptions{})
	defer fixture.close()

	response, err := http.Get(fixture.baseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	body := readResponseBody(t, response)
	if !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("body = %q, want status ok", body)
	}
}

func TestMethodNotAllowedOnKnownPaths(t *testing.T) {
	fixture := newAppFixture(t, appFixtureOptions{})
	defer fixture.close()

	for _, path := range []string{"/", "/health"} {
		request, err := http.NewRequest(http.MethodPost, fixture.baseURL+path, nil)
		if err != nil {
			t.Fatalf("NewRequest %s: %v", path, err)
		}
		response, err := fixture.client.Do(request)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status = %d, want 405", path, response.StatusCode)
		}
	}

	request, err := http.NewRequest(http.MethodGet, fixture.baseURL+"/api/v1/github_webhooks", nil)
	if err != nil {
		t.Fatalf("NewRequest webhook: %v", err)
	}
	response, err := fixture.client.Do(request)
	if err != nil {
		t.Fatalf("GET webhook: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET webhook status = %d, want 405", response.StatusCode)
	}
}

func TestNotFoundForUnknownPaths(t *testing.T) {
	fixture := newAppFixture(t, appFixtureOptions{})
	defer fixture.close()

	response, err := http.Get(fixture.baseURL + "/missing")
	if err != nil {
		t.Fatalf("GET /missing: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.StatusCode)
	}
}

func TestInvalidSignatureReturns401(t *testing.T) {
	fixture := newAppFixture(t, appFixtureOptions{})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-invalid",
		body:       openedPayload(testDefectiveHead),
		signature:  "sha256=00",
	})
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
}

func TestMalformedPayloadReturns400(t *testing.T) {
	fixture := newAppFixture(t, appFixtureOptions{})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-malformed",
		body:       []byte(`{"action":"opened"}`),
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}

func TestMissingHeadersReturns400(t *testing.T) {
	fixture := newAppFixture(t, appFixtureOptions{})
	defer fixture.close()

	body := openedPayload(testDefectiveHead)
	request, err := http.NewRequest(http.MethodPost, fixture.baseURL+"/api/v1/github_webhooks", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("X-Hub-Signature-256", signBody(body))
	response, err := fixture.client.Do(request)
	if err != nil {
		t.Fatalf("POST webhook: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}

func TestOversizedBodyReturns413(t *testing.T) {
	fixture := newAppFixture(t, appFixtureOptions{})
	defer fixture.close()

	oversized := make([]byte, config.MaximumWebhookBytes+1)
	for index := range oversized {
		oversized[index] = 'a'
	}
	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-oversized",
		body:       oversized,
	})
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.StatusCode)
	}
}

func TestUnsupportedEventReturns202(t *testing.T) {
	fixture := newAppFixture(t, appFixtureOptions{})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "push",
		deliveryID: "delivery-push",
		body:       []byte(`{}`),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
}

func TestDuplicateDeliveryReturns202WithoutExtraWork(t *testing.T) {
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent()},
	})
	defer fixture.close()

	body := openedPayload(testDefectiveHead)
	first := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-dup",
		body:       body,
	})
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.StatusCode)
	}
	fixture.waitForClydeCalls(t, 1)

	second := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-dup",
		body:       body,
	})
	if second.StatusCode != http.StatusAccepted {
		t.Fatalf("second status = %d, want 202", second.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if fixture.clydeState.requestCount() != 1 {
		t.Fatalf("clyde requests = %d, want 1", fixture.clydeState.requestCount())
	}
}

func TestSignedWebhookProducesOneReviewCheckAndSilentReconciliation(t *testing.T) {
	defectiveFinding := domain.Finding{
		Path:       testFindingPath,
		StartLine:  3,
		EndLine:    3,
		Title:      "Missing validation",
		Body:       "Validate the webhook payload before enqueue.",
		Importance: config.BlockingImportance,
	}

	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{
			defectiveReviewContent(defectiveFinding),
			approveReviewContent(),
			reconcileResolvedContent("thread-owned"),
		},
	})
	defer fixture.close()

	opened := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-opened",
		body:       openedPayload(testDefectiveHead),
	})
	if opened.StatusCode != http.StatusAccepted {
		t.Fatalf("opened status = %d, want 202", opened.StatusCode)
	}
	fixture.waitForClydeCalls(t, 1)
	fixture.waitForSubmitReviews(t, 1)

	if fixture.githubState.submitReviewCount() != 1 {
		t.Fatalf("submit review count = %d, want 1", fixture.githubState.submitReviewCount())
	}
	firstReview := fixture.githubState.lastSubmitReview()
	if firstReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("first event = %v, want REQUEST_CHANGES", firstReview["event"])
	}
	comments, ok := firstReview["comments"].([]any)
	if !ok || len(comments) != 1 {
		t.Fatalf("first comments = %T(%v), want one inline comment", firstReview["comments"], firstReview["comments"])
	}
	if fixture.githubState.completedCheckCount() != 1 {
		t.Fatalf("completed check count = %d, want 1", fixture.githubState.completedCheckCount())
	}

	duplicate := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-opened",
		body:       openedPayload(testDefectiveHead),
	})
	if duplicate.StatusCode != http.StatusAccepted {
		t.Fatalf("duplicate status = %d, want 202", duplicate.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if fixture.githubState.submitReviewCount() != 1 {
		t.Fatalf("submit review count after duplicate = %d, want 1", fixture.githubState.submitReviewCount())
	}

	fixture.githubState.setHead(testCorrectedHead)
	fixture.githubState.setChangedFiles(correctedChangedFiles())
	fixture.githubState.setFileContent(correctedFileContent())

	synchronize := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-sync",
		body:       synchronizePayload(testCorrectedHead),
	})
	if synchronize.StatusCode != http.StatusAccepted {
		t.Fatalf("synchronize status = %d, want 202", synchronize.StatusCode)
	}
	fixture.waitForClydeCalls(t, 3)
	fixture.waitForSubmitReviews(t, 2)

	secondReview := fixture.githubState.lastSubmitReview()
	if secondReview["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("second event = %v, want APPROVE", secondReview["event"])
	}
	if fixture.githubState.resolveCallCount() != 1 {
		t.Fatalf("resolve count = %d, want 1", fixture.githubState.resolveCallCount())
	}
	if fixture.githubState.lastResolveThreadID() != "thread-owned" {
		t.Fatalf("resolved thread = %q, want thread-owned", fixture.githubState.lastResolveThreadID())
	}
}

type appFixtureOptions struct {
	clydeResponses []string
}

type appFixture struct {
	application *app.App
	server      *httptest.Server
	github      *httptest.Server
	clyde       *httptest.Server
	githubState *githubServerState
	clydeState  *clydeServerState
	baseURL     string
	client      *http.Client
	cancel      context.CancelFunc
}

func newAppFixture(t *testing.T, options appFixtureOptions) *appFixture {
	t.Helper()

	privateKey := testPrivateKey(t)
	githubState := newGitHubServerState(testDefectiveHead)
	githubServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if failForbiddenEndpoint(writer, request) {
			return
		}
		githubState.handle(writer, request)
	}))

	clydeState := &clydeServerState{responses: options.clydeResponses}
	if len(clydeState.responses) == 0 {
		clydeState.responses = []string{approveReviewContent()}
	}
	clydeServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		clydeState.handle(writer, request)
	}))

	apiURL, err := url.Parse(githubServer.URL)
	if err != nil {
		t.Fatalf("Parse github URL: %v", err)
	}
	graphqlURL, err := url.Parse(githubServer.URL + "/graphql")
	if err != nil {
		t.Fatalf("Parse graphql URL: %v", err)
	}
	clydeURL, err := url.Parse(clydeServer.URL)
	if err != nil {
		t.Fatalf("Parse clyde URL: %v", err)
	}

	cfg := config.Config{
		Port:                 "0",
		GitHubAppID:          12345,
		GitHubPrivateKey:     privateKey, // gitleaks:allow
		GitHubWebhookSecret:  []byte(testWebhookSecret), // gitleaks:allow
		GitHubBotLogin:       config.BotLogin,
		GitHubAPIBaseURL:     apiURL,
		GitHubGraphQLURL:     graphqlURL,
		ClydeBaseURL:         clydeURL,
		ClydeAPIKey:          "fixture-clyde-key", // gitleaks:allow
		CFAccessClientID:     "fixture-cf-id", // gitleaks:allow
		CFAccessClientSecret: "fixture-cf-secret", // gitleaks:allow
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := app.New(cfg, githubServer.Client(), clydeServer.Client(), logger)

	httpServer := httptest.NewServer(application.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	application.Start(ctx)

	return &appFixture{
		application: application,
		server:      httpServer,
		github:      githubServer,
		clyde:       clydeServer,
		githubState: githubState,
		clydeState:  clydeState,
		baseURL:     httpServer.URL,
		client:      httpServer.Client(),
		cancel:      cancel,
	}
}

func (fixture *appFixture) close() {
	if fixture.cancel != nil {
		fixture.cancel()
	}
	if fixture.application != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = fixture.application.Shutdown(shutdownCtx)
		cancel()
	}
	if fixture.server != nil {
		fixture.server.Close()
	}
	if fixture.github != nil {
		fixture.github.Close()
	}
	if fixture.clyde != nil {
		fixture.clyde.Close()
	}
}

func (fixture *appFixture) waitForClydeCalls(t *testing.T, count int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fixture.clydeState.requestCount() >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("clyde requests = %d, want >= %d", fixture.clydeState.requestCount(), count)
}

func (fixture *appFixture) waitForSubmitReviews(t *testing.T, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fixture.githubState.submitReviewCount() >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("submit review count = %d, want >= %d", fixture.githubState.submitReviewCount(), count)
}

type webhookRequestOptions struct {
	eventType  string
	deliveryID string
	body       []byte
	signature  string
}

func (fixture *appFixture) postWebhook(t *testing.T, options webhookRequestOptions) *http.Response {
	t.Helper()

	request, err := http.NewRequest(
		http.MethodPost,
		fixture.baseURL+"/api/v1/github_webhooks",
		strings.NewReader(string(options.body)),
	)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if options.eventType != "" {
		request.Header.Set("X-GitHub-Event", options.eventType)
	}
	if options.deliveryID != "" {
		request.Header.Set("X-GitHub-Delivery", options.deliveryID)
	}
	signature := options.signature
	if signature == "" {
		signature = signBody(options.body)
	}
	request.Header.Set("X-Hub-Signature-256", signature)

	response, err := fixture.client.Do(request)
	if err != nil {
		t.Fatalf("POST webhook: %v", err)
	}
	return response
}

func signBody(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testWebhookSecret)) // gitleaks:allow
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func openedPayload(head string) []byte {
	payload := map[string]any{
		"action": "opened",
		"installation": map[string]any{
			"id": float64(testInstallation),
		},
		"repository": map[string]any{
			"name": testRepoName,
			"owner": map[string]any{
				"login": testRepoOwner,
			},
		},
		"pull_request": map[string]any{
			"number": float64(testPRNumber),
			"draft":  false,
			"head": map[string]any{
				"sha": head,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return body
}

func synchronizePayload(head string) []byte {
	payload := map[string]any{
		"action": "synchronize",
		"installation": map[string]any{
			"id": float64(testInstallation),
		},
		"repository": map[string]any{
			"name": testRepoName,
			"owner": map[string]any{
				"login": testRepoOwner,
			},
		},
		"pull_request": map[string]any{
			"number": float64(testPRNumber),
			"draft":  false,
			"head": map[string]any{
				"sha": head,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return body
}

func defectiveReviewContent(finding domain.Finding) string {
	payload := map[string]any{
		"summary":           "Validation missing.",
		"coverage_complete": true,
		"findings": []map[string]any{{
			"path":       finding.Path,
			"start_line": finding.StartLine,
			"end_line":   finding.EndLine,
			"title":      finding.Title,
			"body":       finding.Body,
			"importance": finding.Importance,
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(body)
}

func approveReviewContent() string {
	return `{"summary":"No issues found.","coverage_complete":true,"findings":[]}`
}

func reconcileResolvedContent(threadID string) string {
	payload := map[string]any{
		"resolutions": []map[string]any{{
			"thread_node_id": threadID,
			"resolution":     "resolved",
			"reason":         "fixed",
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(body)
}

func defectiveChangedFiles() []map[string]any {
	patch := strings.Join([]string{
		"@@ -1,1 +1,3 @@",
		" package app",
		"+",
		"+// missing validation",
	}, "\n")
	return []map[string]any{{
		"filename": testFindingPath,
		"status":   "modified",
		"patch":    patch,
	}}
}

func correctedChangedFiles() []map[string]any {
	patch := strings.Join([]string{
		"@@ -1,3 +1,4 @@",
		" package app",
		" ",
		" // missing validation",
		"+if payload == nil { return }",
	}, "\n")
	return []map[string]any{{
		"filename": testFindingPath,
		"status":   "modified",
		"patch":    patch,
	}}
}

func defectiveFileContent() string {
	content := "package app\n\n// missing validation\n"
	return base64.StdEncoding.EncodeToString([]byte(content))
}

func correctedFileContent() string {
	content := "package app\n\n// missing validation\nif payload == nil { return }\n"
	return base64.StdEncoding.EncodeToString([]byte(content))
}

type githubServerState struct {
	mu sync.Mutex

	currentHead     string
	changedFiles    []map[string]any
	fileContent     string
	compareFiles    []map[string]any
	checkRuns       []map[string]any
	nextCheckRunID  int64
	submitReviews   []map[string]any
	listReviewPages [][]map[string]any
	reviewPageIndex int
	threads         []map[string]any
	resolveCalls    []string
	lastResolveID   string
	requests        int32
}

func newGitHubServerState(head string) *githubServerState {
	return &githubServerState{
		currentHead:     head,
		changedFiles:    defectiveChangedFiles(),
		fileContent:     defectiveFileContent(),
		nextCheckRunID:  77,
		listReviewPages: [][]map[string]any{{}},
		compareFiles: []map[string]any{{
			"filename": testFindingPath,
			"status":   "modified",
			"patch": strings.Join([]string{
				"@@ -1,3 +1,4 @@",
				" package app",
				" ",
				" // missing validation",
				"+if payload == nil { return }",
			}, "\n"),
		}},
	}
}

func (state *githubServerState) requestCount() int32 {
	return atomic.LoadInt32(&state.requests)
}

func (state *githubServerState) submitReviewCount() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return len(state.submitReviews)
}

func (state *githubServerState) completedCheckCount() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	count := 0
	for _, item := range state.checkRuns {
		if item["conclusion"] == "success" {
			count++
		}
	}
	return count
}

func (state *githubServerState) lastSubmitReview() map[string]any {
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.submitReviews) == 0 {
		return nil
	}
	return state.submitReviews[len(state.submitReviews)-1]
}

func (state *githubServerState) resolveCallCount() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return len(state.resolveCalls)
}

func (state *githubServerState) lastResolveThreadID() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.lastResolveID
}

func (state *githubServerState) setHead(head string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.currentHead = head
}

func (state *githubServerState) setChangedFiles(files []map[string]any) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.changedFiles = files
}

func (state *githubServerState) setFileContent(content string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.fileContent = content
}

func (state *githubServerState) handle(writer http.ResponseWriter, request *http.Request) {
	atomic.AddInt32(&state.requests, 1)

	if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/access_tokens") {
		writeJSON(writer, http.StatusCreated, map[string]any{
			"token":      "ghs_installation", // gitleaks:allow
			"expires_at": time.Unix(1_700_000_600, 0).UTC().Format(time.RFC3339),
		})
		return
	}

	if request.Method == http.MethodPost && request.URL.Path == "/graphql" {
		state.handleGraphQL(writer, request)
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/check-runs") {
		state.handleListCheckRuns(writer, request)
		return
	}

	if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/check-runs") {
		state.handleCreateCheckRun(writer, request)
		return
	}

	if request.Method == http.MethodPatch && strings.Contains(request.URL.Path, "/check-runs/") {
		state.handleUpdateCheckRun(writer, request)
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/") && strings.HasSuffix(request.URL.Path, "/reviews") {
		state.handleListReviews(writer)
		return
	}

	if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/pulls/") && strings.HasSuffix(request.URL.Path, "/reviews") {
		state.handleSubmitReview(writer, request)
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/") && strings.HasSuffix(request.URL.Path, "/files") {
		state.mu.Lock()
		files := state.changedFiles
		state.mu.Unlock()
		writeJSON(writer, http.StatusOK, files)
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/contents/") {
		state.mu.Lock()
		content := state.fileContent
		state.mu.Unlock()
		writeJSON(writer, http.StatusOK, map[string]any{
			"content":  content,
			"encoding": "base64",
		})
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/compare/") {
		state.mu.Lock()
		files := state.compareFiles
		state.mu.Unlock()
		writeJSON(writer, http.StatusOK, map[string]any{"files": files})
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/") {
		state.mu.Lock()
		head := state.currentHead
		state.mu.Unlock()
		writeJSON(writer, http.StatusOK, map[string]any{
			"number": float64(testPRNumber),
			"draft":  false,
			"title":  "title",
			"body":   "body",
			"head":   map[string]any{"sha": head},
			"base":   map[string]any{"sha": testBaseSHA},
		})
		return
	}

	writer.WriteHeader(http.StatusNotFound)
}

func (state *githubServerState) handleListCheckRuns(writer http.ResponseWriter, request *http.Request) {
	state.mu.Lock()
	defer state.mu.Unlock()
	headSHA := request.URL.Query().Get("head_sha")
	checkName := request.URL.Query().Get("check_name")
	matches := make([]map[string]any, 0)
	for _, item := range state.checkRuns {
		if item["head_sha"] == headSHA && item["name"] == checkName {
			matches = append(matches, item)
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"total_count": len(matches),
		"check_runs":  matches,
	})
}

func (state *githubServerState) handleCreateCheckRun(writer http.ResponseWriter, request *http.Request) {
	body, err := readJSONBody(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	created := map[string]any{
		"id":         float64(state.nextCheckRunID),
		"name":       body["name"],
		"head_sha":   body["head_sha"],
		"status":     body["status"],
		"conclusion": "",
	}
	state.checkRuns = append(state.checkRuns, created)
	writeJSON(writer, http.StatusCreated, created)
}

func (state *githubServerState) handleUpdateCheckRun(writer http.ResponseWriter, request *http.Request) {
	body, err := readJSONBody(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	checkID := strings.TrimPrefix(request.URL.Path, fmt.Sprintf("/repos/%s/%s/check-runs/", testRepoOwner, testRepoName))
	for index, item := range state.checkRuns {
		if fmt.Sprintf("%.0f", item["id"]) != checkID {
			continue
		}
		if status, ok := body["status"].(string); ok && status != "" {
			item["status"] = status
		}
		if conclusion, ok := body["conclusion"].(string); ok {
			item["conclusion"] = conclusion
		}
		state.checkRuns[index] = item
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"id":         float64(state.nextCheckRunID),
		"name":       config.ReviewCheckName,
		"status":     body["status"],
		"conclusion": body["conclusion"],
	})
}

func (state *githubServerState) handleListReviews(writer http.ResponseWriter) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.reviewPageIndex >= len(state.listReviewPages) {
		writeJSON(writer, http.StatusOK, []map[string]any{})
		return
	}
	page := state.listReviewPages[state.reviewPageIndex]
	state.reviewPageIndex++
	writeJSON(writer, http.StatusOK, page)
}

func (state *githubServerState) handleSubmitReview(writer http.ResponseWriter, request *http.Request) {
	body, err := readJSONBody(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	state.mu.Lock()
	state.submitReviews = append(state.submitReviews, body)
	commitID, _ := body["commit_id"].(string)
	reviewBody, _ := body["body"].(string)
	comments, _ := body["comments"].([]any)
	state.listReviewPages = [][]map[string]any{{
		{
			"id":        float64(100 + len(state.submitReviews)),
			"commit_id": commitID,
			"body":      reviewBody,
			"state":     body["event"],
			"user":      map[string]any{"login": config.BotLogin},
		},
	}}
	state.reviewPageIndex = 0
	if len(comments) > 0 {
		comment, ok := comments[0].(map[string]any)
		if ok {
			state.threads = []map[string]any{{
				"id":         "thread-owned",
				"isResolved": false,
				"comments": map[string]any{
					"nodes": []map[string]any{{
						"databaseId": float64(1),
						"body":       comment["body"],
						"path":       comment["path"],
						"line":       comment["line"],
						"startLine":  comment["start_line"],
						"author":     map[string]any{"login": config.BotLogin},
					}},
				},
			}}
		}
	}
	state.mu.Unlock()

	writeJSON(writer, http.StatusOK, map[string]any{
		"id":        float64(42),
		"commit_id": body["commit_id"],
		"state":     body["event"],
		"body":      body["body"],
		"user":      map[string]any{"login": config.BotLogin},
	})
}

func (state *githubServerState) handleGraphQL(writer http.ResponseWriter, request *http.Request) {
	body, err := readJSONBody(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	query, _ := body["query"].(string)
	if strings.Contains(query, "resolveReviewThread") {
		variables, _ := body["variables"].(map[string]any)
		threadID, _ := variables["threadID"].(string)
		state.mu.Lock()
		state.resolveCalls = append(state.resolveCalls, threadID)
		state.lastResolveID = threadID
		state.mu.Unlock()
		writeJSON(writer, http.StatusOK, map[string]any{
			"data": map[string]any{
				"resolveReviewThread": map[string]any{
					"thread": map[string]any{
						"id":         threadID,
						"isResolved": true,
					},
				},
			},
		})
		return
	}

	state.mu.Lock()
	threads := state.threads
	state.mu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"pullRequest": map[string]any{
					"reviewThreads": map[string]any{
						"pageInfo": map[string]any{
							"hasNextPage": false,
							"endCursor":   "",
						},
						"nodes": threads,
					},
				},
			},
		},
	})
}

type clydeServerState struct {
	mu        sync.Mutex
	responses []string
	index     int
	requests  int32
}

func (state *clydeServerState) requestCount() int32 {
	return atomic.LoadInt32(&state.requests)
}

func (state *clydeServerState) handle(writer http.ResponseWriter, request *http.Request) {
	atomic.AddInt32(&state.requests, 1)
	state.mu.Lock()
	content := state.responses[state.index]
	if state.index < len(state.responses)-1 {
		state.index++
	}
	state.mu.Unlock()

	writeJSON(writer, http.StatusOK, map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{
				"content": content,
			},
		}},
	})
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

func testPrivateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return privateKey
}

func readResponseBody(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(body)
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

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

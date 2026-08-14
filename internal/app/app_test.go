package app

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

var integrationTestMu sync.Mutex

func withIntegrationLock(t *testing.T) {
	t.Helper()
	integrationTestMu.Lock()
	t.Cleanup(integrationTestMu.Unlock)
}

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
	withIntegrationLock(t)
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

func TestEndToEndApproveWithCompleteCoverage(t *testing.T) {
	withIntegrationLock(t)

	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent()},
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-approve",
		body:       openedPayload(testDefectiveHead),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()

	fixture.waitForSubmitReviews(t, 1)
	fixture.waitForCheckConclusion(t, "success")
	review := fixture.githubState.lastSubmitReview()
	if review["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("event = %v, want APPROVE", review["event"])
	}
	if fixture.githubState.completedCheckCount() != 1 {
		t.Fatalf("completed check count = %d, want 1", fixture.githubState.completedCheckCount())
	}
	body, _ := review["body"].(string)
	if containsTypographicDash(body) {
		t.Fatalf("review body contains typographic dash: %q", body)
	}
	if fixture.githubState.forbiddenEndpointHits() != 0 {
		t.Fatalf("forbidden endpoint hits = %d, want 0", fixture.githubState.forbiddenEndpointHits())
	}
}

func TestEndToEndRequestChangesWithBlockingFinding(t *testing.T) {
	withIntegrationLock(t)
	blockingFinding := domain.Finding{
		Path:       testFindingPath,
		StartLine:  3,
		EndLine:    3,
		Title:      "Missing validation",
		Body:       "Validate the webhook payload before enqueue.",
		Importance: config.BlockingImportance,
	}

	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{defectiveReviewContent(blockingFinding)},
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-blocking",
		body:       openedPayload(testDefectiveHead),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()

	fixture.waitForSubmitReviews(t, 1)
	review := fixture.githubState.lastSubmitReview()
	if review["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v, want REQUEST_CHANGES", review["event"])
	}
	comments, ok := review["comments"].([]any)
	if !ok || len(comments) != 1 {
		t.Fatalf("comments = %T(%v), want one inline comment", review["comments"], review["comments"])
	}
}

func TestEndToEndMultilineFindingUsesItsOwnFileHunks(t *testing.T) {
	withIntegrationLock(t)
	finding := domain.Finding{
		Path:       "a.go",
		StartLine:  2,
		EndLine:    3,
		Title:      "Broken range",
		Body:       "Both added lines form one defective block.",
		Importance: config.BlockingImportance,
	}

	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{defectiveReviewContent(finding)},
	})
	defer fixture.close()
	fixture.githubState.setChangedFiles([]map[string]any{
		{
			"filename": "a.go",
			"status":   "modified",
			"patch":    "@@ -1,1 +1,3 @@\n package app\n+one\n+two",
		},
		{
			"filename": "z.go",
			"status":   "modified",
			"patch":    "@@ -1,0 +2,1 @@\n+one\n@@ -1,0 +3,1 @@\n+two",
		},
	})

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-file-hunks",
		body:       openedPayload(testDefectiveHead),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()

	fixture.waitForSubmitReviews(t, 1)
	review := fixture.githubState.lastSubmitReview()
	comments, ok := review["comments"].([]any)
	if !ok || len(comments) != 1 {
		t.Fatalf("comments = %T(%v), want one inline comment", review["comments"], review["comments"])
	}
}

func TestEndToEndStaleHeadProducesNoReview(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses:    []string{approveReviewContent()},
		headAfterAnalysis: testCorrectedHead,
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-stale",
		body:       openedPayload(testDefectiveHead),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()

	fixture.waitForClydeCalls(t, 1)
	time.Sleep(200 * time.Millisecond)
	if fixture.githubState.submitReviewCount() != 0 {
		t.Fatalf("submit review count = %d, want 0", fixture.githubState.submitReviewCount())
	}
	if fixture.githubState.lastCheckConclusion() != "cancelled" {
		t.Fatalf("check conclusion = %q, want cancelled", fixture.githubState.lastCheckConclusion())
	}
}

func TestEndToEndGitHubFailureSetsFailedLifecycle(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses:     []string{approveReviewContent()},
		submitReviewStatus: http.StatusInternalServerError,
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-github-fail",
		body:       openedPayload(testDefectiveHead),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()

	fixture.waitForClydeCalls(t, 1)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fixture.githubState.lastCheckConclusion() == "failure" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("check conclusion = %q, want failure", fixture.githubState.lastCheckConclusion())
}

func TestEndToEndFreshAppInstanceMarkerDedup(t *testing.T) {
	withIntegrationLock(t)
	githubState := newGitHubServerState(testDefectiveHead)
	githubServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if failForbiddenEndpoint(writer, request) {
			githubState.recordForbiddenEndpointHit()
			return
		}
		githubState.handle(writer, request)
	}))
	defer githubServer.Close()

	clydeState := newClydeServerState(appFixtureOptions{clydeResponses: []string{approveReviewContent()}})
	clydeServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		clydeState.handle(writer, request)
	}))
	defer clydeServer.Close()

	runWebhook := func(t *testing.T, deliveryID string) {
		t.Helper()
		fixture := newAppFixtureOnServers(t, githubServer, clydeServer, githubState, clydeState)
		defer fixture.close()

		response := fixture.postWebhook(t, webhookRequestOptions{
			eventType:  "pull_request",
			deliveryID: deliveryID,
			body:       openedPayload(testDefectiveHead),
		})
		if response.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", response.StatusCode)
		}
		_ = response.Body.Close()
		fixture.waitForSubmitReviews(t, 1)
	}

	runWebhook(t, "delivery-first-app")
	if githubState.submitReviewCount() != 1 {
		t.Fatalf("submit review count after first app = %d, want 1", githubState.submitReviewCount())
	}

	runWebhook(t, "delivery-second-app")
	time.Sleep(200 * time.Millisecond)
	if clydeState.requestCount() != 1 {
		t.Fatalf("clyde requests after fresh app = %d, want 1", clydeState.requestCount())
	}
	if githubState.submitReviewCount() != 1 {
		t.Fatalf("submit review count after fresh app = %d, want 1", githubState.submitReviewCount())
	}
}

func TestEndToEndMoreThanOneHundredFilesAndThreads(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses:   []string{approveReviewContent()},
		changedFileCount: 101,
		threadCount:      101,
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-large",
		body:       openedPayload(testDefectiveHead),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()

	fixture.waitForSubmitReviews(t, 1)
	if fixture.githubState.listedFilePages() < 2 {
		t.Fatalf("file page fetches = %d, want at least 2", fixture.githubState.listedFilePages())
	}
}

func TestEndToEndNeverCallsIssueCommentOrReplyEndpoints(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{
			defectiveReviewContent(domain.Finding{
				Path:       testFindingPath,
				StartLine:  3,
				EndLine:    3,
				Title:      "Missing validation",
				Body:       "Validate the webhook payload before enqueue.",
				Importance: config.BlockingImportance,
			}),
			approveReviewContent(),
			approveReviewContent(),
		},
		clydeReconcileResponses: []string{reconcileResolvedContent("thread-owned")},
	})
	defer fixture.close()

	opened := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-no-comments",
		body:       openedPayload(testDefectiveHead),
	})
	_ = opened.Body.Close()
	fixture.waitForSubmitReviews(t, 1)

	fixture.githubState.setHead(testCorrectedHead)
	fixture.githubState.setChangedFiles(correctedChangedFiles())
	fixture.githubState.setFileContent(correctedFileContent())

	sync := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-no-comments-sync",
		body:       synchronizePayload(testCorrectedHead),
	})
	_ = sync.Body.Close()

	fixture.waitForClydeCalls(t, 3)
	fixture.waitForSubmitReviews(t, 2)
	if fixture.githubState.forbiddenEndpointHits() != 0 {
		t.Fatalf("forbidden endpoint hits = %d, want 0", fixture.githubState.forbiddenEndpointHits())
	}
}

func TestEndToEndReconciliationFailureIsolation(t *testing.T) {
	withIntegrationLock(t)

	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses:  []string{approveReviewContent()},
		reconcileStatus: http.StatusInternalServerError,
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-reconcile-fail",
		body:       openedPayload(testDefectiveHead),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()

	fixture.waitForSubmitReviews(t, 1)
	fixture.waitForCheckConclusion(t, "success")
}

func TestEndToEndPublishedProseHasNoTypographicDashes(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{typographicReviewContent()},
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-typographic",
		body:       openedPayload(testDefectiveHead),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()

	fixture.waitForSubmitReviews(t, 1)
	review := fixture.githubState.lastSubmitReview()
	body, _ := review["body"].(string)
	if containsTypographicDash(body) {
		t.Fatalf("published review body contains typographic dash: %q", body)
	}
	comments, _ := review["comments"].([]any)
	for _, item := range comments {
		comment, ok := item.(map[string]any)
		if !ok {
			continue
		}
		commentBody, _ := comment["body"].(string)
		if containsTypographicDash(commentBody) {
			t.Fatalf("published inline body contains typographic dash: %q", commentBody)
		}
	}
}

func TestSignedWebhookProducesOneReviewCheckAndSilentReconciliation(t *testing.T) {
	withIntegrationLock(t)
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
		},
		clydeReconcileResponses: []string{reconcileResolvedContent("thread-owned")},
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
	fixture.waitForCheckConclusion(t, "success")
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
	fixture.waitForResolveCalls(t, 1)
	if fixture.githubState.resolveCallCount() != 1 {
		t.Fatalf("resolve count = %d, want 1", fixture.githubState.resolveCallCount())
	}
	if fixture.githubState.lastResolveThreadID() != "thread-owned" {
		t.Fatalf("resolved thread = %q, want thread-owned", fixture.githubState.lastResolveThreadID())
	}
}

type appFixtureOptions struct {
	clydeResponses          []string
	clydeReconcileResponses []string
	clydeStatus             int
	reconcileStatus         int
	headAfterAnalysis       string
	submitReviewStatus      int
	changedFileCount        int
	threadCount             int
}

type appFixture struct {
	application *App
	server      *httptest.Server
	github      *httptest.Server
	clyde       *httptest.Server
	githubState *githubServerState
	clydeState  *clydeServerState
	baseURL     string
	client      *http.Client
	cancel      context.CancelFunc
	ownsGitHub  bool
	ownsClyde   bool
}

func newAppFixture(t *testing.T, options appFixtureOptions) *appFixture {
	t.Helper()

	githubState := newGitHubServerState(testDefectiveHead)
	applyGitHubFixtureOptions(githubState, options)

	githubServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if failForbiddenEndpoint(writer, request) {
			githubState.recordForbiddenEndpointHit()
			return
		}
		githubState.handle(writer, request)
	}))

	clydeState := newClydeServerState(options)
	clydeServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		clydeState.handle(writer, request)
	}))

	fixture := wireAppFixture(t, githubServer, clydeServer, githubState, clydeState)
	fixture.ownsGitHub = true
	fixture.ownsClyde = true
	return fixture
}

func newAppFixtureOnServers(
	t *testing.T,
	githubServer *httptest.Server,
	clydeServer *httptest.Server,
	githubState *githubServerState,
	clydeState *clydeServerState,
) *appFixture {
	t.Helper()
	return wireAppFixture(t, githubServer, clydeServer, githubState, clydeState)
}

func wireAppFixture(
	t *testing.T,
	githubServer *httptest.Server,
	clydeServer *httptest.Server,
	githubState *githubServerState,
	clydeState *clydeServerState,
) *appFixture {
	t.Helper()

	privateKey := testPrivateKey(t)
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
		GitHubPrivateKey:     privateKey,                // gitleaks:allow
		GitHubWebhookSecret:  []byte(testWebhookSecret), // gitleaks:allow
		GitHubBotLogin:       config.BotLogin,
		GitHubAPIBaseURL:     apiURL,
		GitHubGraphQLURL:     graphqlURL,
		ClydeBaseURL:         clydeURL,
		ClydeAPIKey:          "fixture-clyde-key", // gitleaks:allow
		CFAccessClientID:     "fixture-cf-id",     // gitleaks:allow
		CFAccessClientSecret: "fixture-cf-secret", // gitleaks:allow
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	application := New(cfg, githubServer.Client(), clydeServer.Client(), logger)

	httpServer := httptest.NewServer(application.server.Handler)
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

func applyGitHubFixtureOptions(state *githubServerState, options appFixtureOptions) {
	if options.changedFileCount > 0 {
		state.setChangedFiles(buildChangedFiles(options.changedFileCount))
	}
	if options.threadCount > 0 {
		state.setThreads(buildReviewThreads(options.threadCount))
	}
	if options.headAfterAnalysis != "" {
		state.headAfterAnalysis = options.headAfterAnalysis
	}
	if options.submitReviewStatus != 0 {
		state.submitReviewStatus = options.submitReviewStatus
	}
}

func newClydeServerState(options appFixtureOptions) *clydeServerState {
	state := &clydeServerState{
		responses:          options.clydeResponses,
		reconcileResponses: options.clydeReconcileResponses,
		status:             options.clydeStatus,
		reconcileStatus:    options.reconcileStatus,
	}
	if len(state.responses) == 0 && state.status == 0 {
		state.responses = []string{approveReviewContent()}
	}
	return state
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
	if fixture.ownsGitHub && fixture.github != nil {
		fixture.github.Close()
	}
	if fixture.ownsClyde && fixture.clyde != nil {
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

func (fixture *appFixture) waitForCheckConclusion(t *testing.T, conclusion string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fixture.githubState.lastCheckConclusion() == conclusion {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("check conclusion = %q, want %q", fixture.githubState.lastCheckConclusion(), conclusion)
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

func (fixture *appFixture) waitForResolveCalls(t *testing.T, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fixture.githubState.resolveCallCount() >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("resolve count = %d, want >= %d", fixture.githubState.resolveCallCount(), count)
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

func typographicReviewContent() string {
	return `{"summary":"Issue — details","coverage_complete":true,"findings":[{"path":"internal/app/handler.go","start_line":3,"end_line":3,"title":"Title – note","body":"Body — impact","importance":5}]}`
}

func buildChangedFiles(count int) []map[string]any {
	files := make([]map[string]any, 0, count)
	for index := range count {
		path := fmt.Sprintf("internal/pkg/file_%03d.go", index)
		files = append(files, map[string]any{
			"filename": path,
			"status":   "modified",
			"patch": strings.Join([]string{
				"@@ -1,1 +1,2 @@",
				" package pkg",
				"+// change",
			}, "\n"),
		})
	}
	return files
}

func buildReviewThreads(count int) []map[string]any {
	threads := make([]map[string]any, 0, count)
	for index := range count {
		threads = append(threads, map[string]any{
			"id":         fmt.Sprintf("thread-%d", index),
			"isResolved": true,
			"comments": map[string]any{
				"nodes": []map[string]any{{
					"databaseId": float64(index + 1),
					"body":       "resolved",
					"path":       "internal/app/handler.go",
					"line":       float64(3),
					"startLine":  float64(3),
					"author":     map[string]any{"login": config.BotLogin},
				}},
			},
		})
	}
	return threads
}

func containsTypographicDash(value string) bool {
	for _, character := range value {
		switch character {
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
			return true
		}
	}
	return false
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

	currentHead          string
	headAfterAnalysis    string
	changedFiles         []map[string]any
	changedFilePages     [][]map[string]any
	changedFilePageIndex int
	fileContent          string
	compareFiles         []map[string]any
	checkRuns            []map[string]any
	nextCheckRunID       int64
	submitReviews        []map[string]any
	submitReviewStatus   int
	listReviewPages      [][]map[string]any
	reviewPageIndex      int
	threads              []map[string]any
	threadPages          [][]map[string]any
	threadPageIndex      int
	resolveCalls         []string
	lastResolveID        string
	pullRequestReads     int32
	forbiddenHits        int32
	filePageFetches      int32
	threadPageFetches    int32
	requests             int32
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

func (state *githubServerState) forbiddenEndpointHits() int32 {
	return atomic.LoadInt32(&state.forbiddenHits)
}

func (state *githubServerState) recordForbiddenEndpointHit() {
	atomic.AddInt32(&state.forbiddenHits, 1)
}

func (state *githubServerState) lastCheckConclusion() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.checkRuns) == 0 {
		return ""
	}
	conclusion, _ := state.checkRuns[len(state.checkRuns)-1]["conclusion"].(string)
	return conclusion
}

func (state *githubServerState) listedFilePages() int32 {
	return atomic.LoadInt32(&state.filePageFetches)
}

func (state *githubServerState) listedThreadPages() int32 {
	return atomic.LoadInt32(&state.threadPageFetches)
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
	state.changedFilePages = paginateMapPages(files, 100)
	state.changedFilePageIndex = 0
}

func (state *githubServerState) setFileContent(content string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.fileContent = content
}

func (state *githubServerState) setCompareFiles(files []map[string]any) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.compareFiles = files
}

func (state *githubServerState) setThreads(threads []map[string]any) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.threads = threads
	state.threadPages = paginateMapPages(threads, 100)
	state.threadPageIndex = 0
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

	if request.Method == http.MethodGet &&
		strings.Contains(request.URL.Path, "/commits/") &&
		strings.HasSuffix(request.URL.Path, "/check-runs") {
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
		state.handleListChangedFiles(writer, request)
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
		readCount := atomic.AddInt32(&state.pullRequestReads, 1)
		state.mu.Lock()
		head := state.currentHead
		if readCount > 1 && state.headAfterAnalysis != "" {
			head = state.headAfterAnalysis
		}
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
	pathWithoutSuffix := strings.TrimSuffix(request.URL.Path, "/check-runs")
	headSHA := pathWithoutSuffix[strings.LastIndex(pathWithoutSuffix, "/")+1:]
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
	submitStatus := state.submitReviewStatus
	state.mu.Unlock()
	if submitStatus != 0 && submitStatus != http.StatusOK {
		writeJSON(writer, submitStatus, map[string]any{
			"message": "submit review failed",
		})
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
	threadPages := state.threadPages
	threadPageIndex := state.threadPageIndex
	state.mu.Unlock()

	if len(threadPages) > 0 {
		atomic.AddInt32(&state.threadPageFetches, 1)
		if threadPageIndex >= len(threadPages) {
			writeJSON(writer, http.StatusOK, map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequest": map[string]any{
							"reviewThreads": map[string]any{
								"pageInfo": map[string]any{
									"hasNextPage": false,
									"endCursor":   "",
								},
								"nodes": []map[string]any{},
							},
						},
					},
				},
			})
			return
		}
		page := threadPages[threadPageIndex]
		state.mu.Lock()
		state.threadPageIndex++
		hasNext := state.threadPageIndex < len(threadPages)
		state.mu.Unlock()
		writeJSON(writer, http.StatusOK, map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"pullRequest": map[string]any{
						"reviewThreads": map[string]any{
							"pageInfo": map[string]any{
								"hasNextPage": hasNext,
								"endCursor":   fmt.Sprintf("cursor-%d", threadPageIndex),
							},
							"nodes": page,
						},
					},
				},
			},
		})
		return
	}

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

func (state *githubServerState) handleListChangedFiles(writer http.ResponseWriter, request *http.Request) {
	state.mu.Lock()
	pages := state.changedFilePages
	pageIndex := state.changedFilePageIndex
	files := state.changedFiles
	state.mu.Unlock()

	if len(pages) > 0 {
		atomic.AddInt32(&state.filePageFetches, 1)
		if pageIndex >= len(pages) {
			writeJSON(writer, http.StatusOK, []map[string]any{})
			return
		}
		page := pages[pageIndex]
		state.mu.Lock()
		state.changedFilePageIndex++
		hasNext := state.changedFilePageIndex < len(pages)
		state.mu.Unlock()
		if hasNext {
			next := fmt.Sprintf("http://%s%s", request.Host, request.URL.Path)
			if request.URL.RawQuery != "" {
				next += "?" + request.URL.RawQuery + "&page=2"
			} else {
				next += "?page=2"
			}
			writer.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
		}
		writeJSON(writer, http.StatusOK, page)
		return
	}

	writeJSON(writer, http.StatusOK, files)
}

func paginateMapPages(items []map[string]any, pageSize int) [][]map[string]any {
	if len(items) == 0 {
		return nil
	}
	pages := make([][]map[string]any, 0)
	for start := 0; start < len(items); start += pageSize {
		end := start + pageSize
		if end > len(items) {
			end = len(items)
		}
		pages = append(pages, items[start:end])
	}
	return pages
}

type clydeServerState struct {
	mu                 sync.Mutex
	responses          []string
	reconcileResponses []string
	index              int
	reconcileIndex     int
	status             int
	reconcileStatus    int
	requests           int32
	reconcileRequests  int32
}

func (state *clydeServerState) requestCount() int32 {
	return atomic.LoadInt32(&state.requests)
}

func (state *clydeServerState) reconcileRequestCount() int32 {
	return atomic.LoadInt32(&state.reconcileRequests)
}

func (state *clydeServerState) handle(writer http.ResponseWriter, request *http.Request) {
	atomic.AddInt32(&state.requests, 1)

	body, err := readJSONBody(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	isReconcile := false
	if responseFormat, ok := body["response_format"].(map[string]any); ok {
		if jsonSchema, ok := responseFormat["json_schema"].(map[string]any); ok {
			if name, ok := jsonSchema["name"].(string); ok && name == "thread_resolutions" {
				isReconcile = true
				atomic.AddInt32(&state.reconcileRequests, 1)
			}
		}
	}

	state.mu.Lock()
	status := state.status
	if isReconcile && state.reconcileStatus != 0 {
		status = state.reconcileStatus
	}
	var content string
	if isReconcile {
		if len(state.reconcileResponses) == 0 {
			content = reconcileResolvedContent("thread-owned")
		} else {
			content = state.reconcileResponses[state.reconcileIndex]
			if state.reconcileIndex < len(state.reconcileResponses)-1 {
				state.reconcileIndex++
			}
		}
	} else {
		content = state.responses[state.index]
		if state.index < len(state.responses)-1 {
			state.index++
		}
	}
	state.mu.Unlock()

	if status != 0 && status != http.StatusOK {
		http.Error(writer, "clyde request failed", status)
		return
	}

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

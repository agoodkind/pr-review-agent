package app

import (
	"bytes"
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/gklog/correlation"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

const (
	testWebhookSecret     = "test-webhook-secret" // gitleaks:allow
	testBotLogin          = "test-review-agent[bot]"
	testDefectiveHead     = "a3c4f1cac7f595bc824704b9d2a1f1191630dc32"
	testCorrectedHead     = "b4d5e2dbd8f606cd935815c0e3b2f2202741ed43"
	testBaseSHA           = "c5e6f3ece9f717de046926d1f4c3f3313852fe54"
	testFindingPath       = "internal/app/handler.go"
	testRepoOwner         = "agoodkind"
	testRepoName          = "pr-review-agent"
	testPRNumber          = 42
	testInstallation      = int64(99)
	testMinimumImportance = 7
	testReviewModel       = "fixture-review-model"
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

// A resolved review thread must move the verdict with no push: the
// pull_request_review_thread delivery at the already reviewed head finds the
// standing CHANGES_REQUESTED disagreeing with all-resolved thread state and
// submits an APPROVE.
func TestResolvedThreadWebhookRefreshesTheVerdictWithoutAPush(t *testing.T) {
	withIntegrationLock(t)
	defectiveFinding := domain.Finding{
		Path:       testFindingPath,
		StartLine:  3,
		EndLine:    3,
		Title:      "Missing validation",
		Body:       "Validate the webhook payload before enqueue.",
		Importance: testMinimumImportance,
	}
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{defectiveReviewContent(defectiveFinding)},
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
	fixture.waitForSubmitReviews(t, 1)
	fixture.waitForCheckConclusion(t, "success")
	blocking := fixture.githubState.lastSubmitReview()
	if blocking["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("first event = %v, want REQUEST_CHANGES", blocking["event"])
	}

	// A person resolves the finding's thread on GitHub; no commit is pushed.
	fixture.githubState.markAllThreadsResolved()

	resolved := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request_review_thread",
		deliveryID: "delivery-thread-resolved",
		body:       reviewThreadPayload("resolved", testDefectiveHead),
	})
	if resolved.StatusCode != http.StatusAccepted {
		t.Fatalf("resolved status = %d, want 202", resolved.StatusCode)
	}

	fixture.waitForSubmitReviews(t, 2)
	approval := fixture.githubState.lastSubmitReview()
	if approval["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("refreshed event = %v, want APPROVE", approval["event"])
	}
	if commit := approval["commit_id"]; commit != testDefectiveHead {
		t.Fatalf("refreshed commit = %v, want the reviewed head", commit)
	}
	if fixture.githubState.lastCheckConclusion() != "success" {
		t.Fatalf("conclusion = %q, want success kept", fixture.githubState.lastCheckConclusion())
	}
}

// A resolution delivery at a head with chunks still pending re-runs the owed
// work; it must not approve a head nobody finished reading.
func TestResolvedThreadWebhookDoesNotApproveAHeadWithPendingChunks(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeStatus: http.StatusInternalServerError,
	})
	defer fixture.close()

	opened := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-opened-pending",
		body:       openedPayload(testDefectiveHead),
	})
	if opened.StatusCode != http.StatusAccepted {
		t.Fatalf("opened status = %d, want 202", opened.StatusCode)
	}
	fixture.waitForClydeCalls(t, 1)
	fixture.waitForCheckCompletions(t, 1)
	fixture.waitForCheckConclusion(t, "action_required")

	resolved := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request_review_thread",
		deliveryID: "delivery-thread-pending",
		body:       reviewThreadPayload("resolved", testDefectiveHead),
	})
	if resolved.StatusCode != http.StatusAccepted {
		t.Fatalf("resolved status = %d, want 202", resolved.StatusCode)
	}
	// The second run has to be finished before anything is asserted about it.
	// Its model call starting says only that it began, and the conclusion it
	// leaves is the one the first run already left, so neither tells the two
	// runs apart. The completion count does.
	fixture.waitForClydeCalls(t, 2)
	fixture.waitForCheckCompletions(t, 2)
	if conclusion := fixture.githubState.lastCheckConclusion(); conclusion != "action_required" {
		t.Fatalf("conclusion = %q, want action_required after the second run", conclusion)
	}
	if count := fixture.githubState.submitReviewCount(); count != 0 {
		t.Fatalf("submit review count = %d, want 0 with chunks pending", count)
	}
}

// A person has no other way to re-trigger a review. A run that died leaves a
// red check nothing clears, because only a pull request webhook starts a run,
// and a configuration change reaches the container only when it restarts. A
// label answers both, so it has to review a head every other delivery would be
// suppressed at, and review the whole pull request rather than a delta.
func TestALabelReviewsAnAlreadyReviewedHeadAgainInFull(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent()},
	})
	defer fixture.close()

	opened := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-label-opened",
		body:       openedPayload(testDefectiveHead),
	})
	if opened.StatusCode != http.StatusAccepted {
		t.Fatalf("opened status = %d, want 202", opened.StatusCode)
	}
	_ = opened.Body.Close()
	fixture.waitForSubmitReviews(t, 1)
	fixture.waitForCheckCompletions(t, 1)
	// This head now carries both gates a redelivery is stopped by: the review
	// marker on the submitted review, and a durable state naming it reviewed.
	fixture.waitForSummaryHead(t, testDefectiveHead)

	labeled := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-label-forced",
		body:       labeledPayload(testDefectiveHead, domain.ForceReviewLabelPrefix+"rerun"),
	})
	if labeled.StatusCode != http.StatusAccepted {
		t.Fatalf("labeled status = %d, want 202", labeled.StatusCode)
	}
	_ = labeled.Body.Close()

	fixture.waitForClydeCalls(t, 2)
	fixture.waitForCheckCompletions(t, 2)
	if fixture.githubState.submitReviewCount() != 2 {
		t.Fatalf("submit review count = %d, want 2: the label must publish a second review",
			fixture.githubState.submitReviewCount())
	}
	// A run measuring from the baseline the first run wrote would compare that
	// commit against itself. The forced run measures from nothing, so it lists
	// the whole pull request the way first contact does.
	if ranges := fixture.githubState.comparedRanges(); ranges != 0 {
		t.Fatalf("compare range fetches = %d, want 0: a forced run reviews the whole pull request", ranges)
	}
	summary := fixture.githubState.summaryCommentBody()
	if !strings.Contains(summary, "Triggered by a `"+domain.ForceReviewLabelPrefix+"` label") {
		t.Fatalf("summary comment does not say the label triggered the run: %q", summary)
	}
	if !strings.Contains(summary, "reviewed the whole pull request") {
		t.Fatalf("summary comment does not say the run covered the whole pull request: %q", summary)
	}
}

// Only this service's own labels re-trigger a review. Any other label a person
// adds is answered and ignored, exactly like an unsupported action, because a
// label is otherwise an ordinary thing to put on a pull request.
func TestALabelThisServiceDoesNotOwnChangesNothing(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent()},
	})
	defer fixture.close()

	opened := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-other-label-opened",
		body:       openedPayload(testDefectiveHead),
	})
	if opened.StatusCode != http.StatusAccepted {
		t.Fatalf("opened status = %d, want 202", opened.StatusCode)
	}
	_ = opened.Body.Close()
	fixture.waitForSubmitReviews(t, 1)
	fixture.waitForCheckCompletions(t, 1)
	before := fixture.githubState.summaryCommentBody()

	labeled := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-other-label",
		body:       labeledPayload(testDefectiveHead, "needs-review"),
	})
	if labeled.StatusCode != http.StatusAccepted {
		t.Fatalf("labeled status = %d, want 202", labeled.StatusCode)
	}
	_ = labeled.Body.Close()

	time.Sleep(300 * time.Millisecond)
	if calls := fixture.clydeState.requestCount(); calls != 1 {
		t.Fatalf("clyde requests = %d, want 1: a label this service does not own reviews nothing", calls)
	}
	if count := fixture.githubState.submitReviewCount(); count != 1 {
		t.Fatalf("submit review count = %d, want 1", count)
	}
	if after := fixture.githubState.summaryCommentBody(); after != before {
		t.Fatalf("summary comment changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// The container reads its configuration once, at start, so a value corrected
// after it booted did not reach a running instance and a chunk timeout lowered
// by mistake governed real pull requests until the process was replaced. The
// worker attaches the current values to each delivery instead, and the review
// runs on what arrived with the work rather than on what the process booted
// with.
//
// The second delivery is half the point. It carries nothing, and it has to run
// on the process values, because that is what lets a worker and a container at
// different versions work together at all.
func TestReviewSettingsTravelWithTheDeliveryAndFallBackWithoutIt(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent(), approveReviewContent()},
	})
	defer fixture.close()

	carried := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-settings-carried",
		body:       openedPayload(testDefectiveHead),
		settings:   `{"minimum_importance":3,"max_files":7,"max_chunks":5,"chunk_timeout":"9s"}`,
	})
	if carried.StatusCode != http.StatusAccepted {
		t.Fatalf("carried status = %d, want 202", carried.StatusCode)
	}
	_ = carried.Body.Close()
	fixture.waitForCheckCompletions(t, 1)

	absent := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-settings-absent",
		body:       labeledPayload(testDefectiveHead, domain.ForceReviewLabelPrefix+"rerun"),
	})
	if absent.StatusCode != http.StatusAccepted {
		t.Fatalf("absent status = %d, want 202", absent.StatusCode)
	}
	_ = absent.Body.Close()
	fixture.waitForCheckCompletions(t, 2)

	started := fixture.logLinesContaining("review job started")
	if len(started) != 2 {
		t.Fatalf("review job started lines = %d, want 2", len(started))
	}
	if started[0]["chunk_timeout"] != float64(9*time.Second) {
		t.Fatalf("carried chunk_timeout = %v, want the 9s the delivery asked for",
			started[0]["chunk_timeout"])
	}
	if started[0]["minimum_importance"] != float64(3) {
		t.Fatalf("carried minimum_importance = %v, want 3", started[0]["minimum_importance"])
	}
	if started[0]["max_files"] != float64(7) || started[0]["max_chunks"] != float64(5) {
		t.Fatalf("carried budgets = %v/%v, want 7/5", started[0]["max_files"], started[0]["max_chunks"])
	}

	if started[1]["chunk_timeout"] != float64(time.Minute) {
		t.Fatalf("fallback chunk_timeout = %v, want the process value %v",
			started[1]["chunk_timeout"], time.Minute)
	}
	if started[1]["minimum_importance"] != float64(testMinimumImportance) {
		t.Fatalf("fallback minimum_importance = %v, want the process value %d",
			started[1]["minimum_importance"], testMinimumImportance)
	}
	if started[1]["max_files"] != float64(1000) || started[1]["max_chunks"] != float64(1000) {
		t.Fatalf("fallback budgets = %v/%v, want the process values 1000/1000",
			started[1]["max_files"], started[1]["max_chunks"])
	}
}

// The webhook signature covers the request body and nothing else, so a verified
// body is no authority over a header travelling beside it. Reading the tuning
// values on that basis let anyone able to put a request in front of this service
// choose them: a chunk timeout that fails every review, or an importance floor
// that suppresses every finding while the verdict still reports success.
//
// The values carry their own signature, over themselves and the body together.
// Altering them here leaves the body verifying exactly as before, which is the
// whole point: the delivery is still real, and only what it claimed about
// configuration is refused.
func TestSettingsAlteredBesideAVerifiedBodyAreRefused(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent()},
	})
	defer fixture.close()

	body := openedPayload(testDefectiveHead)
	honest := `{"chunk_timeout":"9s","minimum_importance":3}`
	altered := `{"chunk_timeout":"1ms","minimum_importance":11}`

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-settings-altered",
		body:       body,
		settings:   altered,
		// A signature that verifies, over the values somebody else sent.
		settingsSignature: signReviewSettings(honest, body),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: the body verified, so the review is not in doubt",
			response.StatusCode)
	}
	_ = response.Body.Close()
	fixture.waitForCheckCompletions(t, 1)

	started := fixture.logLinesContaining("review job started")
	if len(started) != 1 {
		t.Fatalf("review job started lines = %d, want 1", len(started))
	}
	if started[0]["chunk_timeout"] != float64(time.Minute) {
		t.Fatalf("chunk_timeout = %v, want the process value: the altered one must be refused",
			started[0]["chunk_timeout"])
	}
	if started[0]["minimum_importance"] != float64(testMinimumImportance) {
		t.Fatalf("minimum_importance = %v, want the process value %d",
			started[0]["minimum_importance"], testMinimumImportance)
	}
	carried, ok := started[0]["settings_carried"].([]any)
	if !ok || len(carried) != 0 {
		t.Fatalf("settings_carried = %v, want none: nothing it carried was applied",
			started[0]["settings_carried"])
	}
}

// A signature is worth nothing on any other delivery, because the body is part
// of what it covers. Lifting a valid pair off one request and replaying it in
// front of another is the same attack one step along, and it fails the same way.
func TestSettingsLiftedFromAnotherDeliveryAreRefused(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent()},
	})
	defer fixture.close()

	settings := `{"chunk_timeout":"9s"}`
	otherBody := labeledPayload(testDefectiveHead, domain.ForceReviewLabelPrefix+"other")

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-settings-lifted",
		body:       openedPayload(testDefectiveHead),
		settings:   settings,
		// Valid, for the same values, against a different delivery's body.
		settingsSignature: signReviewSettings(settings, otherBody),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()
	fixture.waitForCheckCompletions(t, 1)

	started := fixture.logLinesContaining("review job started")
	if len(started) != 1 {
		t.Fatalf("review job started lines = %d, want 1", len(started))
	}
	if started[0]["chunk_timeout"] != float64(time.Minute) {
		t.Fatalf("chunk_timeout = %v, want the process value: a signature from another delivery is not one",
			started[0]["chunk_timeout"])
	}
}

// The point of carrying the values is that a change is visible when it takes
// effect, so the run has to say which came with the delivery and which it booted
// with. A start line reporting only the resolved numbers leaves a reader unable
// to tell a correction that landed from one that never arrived.
//
// It names what the run took rather than what the delivery sent: a value the
// resolution refuses falls back like one that never arrived, so naming it as
// carried would describe a run nobody is having.
func TestTheRunReportsWhichSettingsCameWithTheDelivery(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent(), approveReviewContent()},
	})
	defer fixture.close()

	partial := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-settings-partial",
		body:       openedPayload(testDefectiveHead),
		// A timeout and a refused budget: one is taken, the other falls back. The
		// refused one is negative rather than zero, so a resolution that honored
		// anything the delivery merely mentioned would take it and be caught.
		settings: `{"chunk_timeout":"11s","max_chunks":-5}`,
	})
	if partial.StatusCode != http.StatusAccepted {
		t.Fatalf("partial status = %d, want 202", partial.StatusCode)
	}
	_ = partial.Body.Close()
	fixture.waitForCheckCompletions(t, 1)

	none := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-settings-none",
		body:       labeledPayload(testDefectiveHead, domain.ForceReviewLabelPrefix+"rerun"),
	})
	if none.StatusCode != http.StatusAccepted {
		t.Fatalf("none status = %d, want 202", none.StatusCode)
	}
	_ = none.Body.Close()
	fixture.waitForCheckCompletions(t, 2)

	started := fixture.logLinesContaining("review job started")
	if len(started) != 2 {
		t.Fatalf("review job started lines = %d, want 2", len(started))
	}
	carried, ok := started[0]["settings_carried"].([]any)
	if !ok {
		t.Fatalf("settings_carried = %v, want a list", started[0]["settings_carried"])
	}
	if len(carried) != 1 || carried[0] != "chunk_timeout" {
		t.Fatalf("settings_carried = %v, want only the timeout the run took", carried)
	}
	if started[0]["max_chunks"] != float64(1000) {
		t.Fatalf("max_chunks = %v, want the process value the refused budget fell back to",
			started[0]["max_chunks"])
	}
	empty, ok := started[1]["settings_carried"].([]any)
	if !ok {
		t.Fatalf("settings_carried = %v, want a list", started[1]["settings_carried"])
	}
	if len(empty) != 0 {
		t.Fatalf("settings_carried = %v, want none: the delivery carried nothing", empty)
	}
}

// Configuration is exactly what an attacker would want to set, and these values
// ride beside the signed body rather than inside it. A request whose signature
// does not verify is refused before anything it carried is read, so nothing it
// asked for reaches a review, because no review starts.
func TestAnUnverifiedDeliveryAppliesNothingItCarried(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-settings-forged",
		body:       openedPayload(testDefectiveHead),
		signature:  "sha256=" + strings.Repeat("0", 64),
		settings:   `{"minimum_importance":1,"max_files":1,"max_chunks":1,"chunk_timeout":"1ms"}`,
	})
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
	_ = response.Body.Close()

	if started := fixture.logLinesContaining("review job started"); len(started) != 0 {
		t.Fatalf("review job started lines = %d, want none: a forged delivery started a review", len(started))
	}
}

// A value that would disable a budget or a clock is not honored, whatever sent
// it. A zero budget refuses every real delta while admitting an empty one, and a
// zero timeout ends every model call before it begins, so the floor the
// constructor applies to configuration has to hold on a delivery too.
func TestANonPositiveCarriedSettingFallsBackToTheProcessValue(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent()},
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-settings-zero",
		body:       openedPayload(testDefectiveHead),
		settings:   `{"minimum_importance":0,"max_files":0,"max_chunks":-1,"chunk_timeout":"0s"}`,
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()
	fixture.waitForCheckCompletions(t, 1)

	started := fixture.logLinesContaining("review job started")
	if len(started) != 1 {
		t.Fatalf("review job started lines = %d, want 1", len(started))
	}
	if started[0]["chunk_timeout"] != float64(time.Minute) {
		t.Fatalf("chunk_timeout = %v, want the process value %v", started[0]["chunk_timeout"], time.Minute)
	}
	if started[0]["max_chunks"] != float64(1000) || started[0]["max_files"] != float64(1000) {
		t.Fatalf("budgets = %v/%v, want the process values 1000/1000",
			started[0]["max_files"], started[0]["max_chunks"])
	}
	if conclusion := fixture.githubState.lastCheckConclusion(); conclusion != "success" {
		t.Fatalf("conclusion = %q, want success: a zero budget must not decline the delta", conclusion)
	}
}

// An importance floor has a ceiling as well as a floor, because findings are
// rated one through ten. A floor above ten clears no finding at all while the
// run still reports a successful verdict, which reads to a person as a pull
// request with no defects rather than as a threshold nothing could meet.
func TestAnImportanceFloorAboveTheScaleFallsBackToTheProcessValue(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent()},
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-settings-above-scale",
		body:       openedPayload(testDefectiveHead),
		settings:   `{"minimum_importance":11}`,
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()
	fixture.waitForCheckCompletions(t, 1)

	started := fixture.logLinesContaining("review job started")
	if len(started) != 1 {
		t.Fatalf("review job started lines = %d, want 1", len(started))
	}
	if started[0]["minimum_importance"] != float64(7) {
		t.Fatalf("minimum_importance = %v, want the process value 7: a floor above ten clears nothing",
			started[0]["minimum_importance"])
	}
}

// A label decides that a review runs, never how it runs. Anyone with triage
// access on a repository can add one, so a label that could set a timeout, a
// budget, or a model would be fault injection reaching production. The text
// after the prefix is an opaque identifier: it is recorded on one log line so
// an operator can tie the run back to the label they added, and read nowhere
// else.
func TestAConfigurationShapedLabelChangesNoTimeoutAndNoBudget(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent()},
	})
	defer fixture.close()

	labels := []string{
		domain.ForceReviewLabelPrefix + "REVIEW_CHUNK_TIMEOUT=1s",
		domain.ForceReviewLabelPrefix + "REVIEW_MAX_CHUNKS=0",
		domain.ForceReviewLabelPrefix + "REVIEW_MIN_IMPORTANCE=1",
		domain.ForceReviewLabelPrefix + "REVIEW_MODEL=other-model",
	}
	for index, label := range labels {
		response := fixture.postWebhook(t, webhookRequestOptions{
			eventType:  "pull_request",
			deliveryID: fmt.Sprintf("delivery-config-label-%d", index),
			body:       labeledPayload(testDefectiveHead, label),
		})
		if response.StatusCode != http.StatusAccepted {
			t.Fatalf("%s status = %d, want 202", label, response.StatusCode)
		}
		_ = response.Body.Close()
		fixture.waitForCheckCompletions(t, int32(index+1))

		// A budget the label had lowered would decline this delta instead of
		// reviewing it, and the check would stop short of success.
		if conclusion := fixture.githubState.lastCheckConclusion(); conclusion != "success" {
			t.Fatalf("%s conclusion = %q, want success: the review budgets are unchanged",
				label, conclusion)
		}
	}

	// The run start line reports the values each run actually used.
	started := fixture.logLinesContaining("review job started")
	if len(started) != len(labels) {
		t.Fatalf("review job started lines = %d, want %d", len(started), len(labels))
	}
	for _, record := range started {
		if record["chunk_timeout"] != float64(time.Minute) {
			t.Fatalf("chunk_timeout = %v, want the configured %v", record["chunk_timeout"], time.Minute)
		}
		if record["minimum_importance"] != float64(testMinimumImportance) {
			t.Fatalf("minimum_importance = %v, want the configured %d",
				record["minimum_importance"], testMinimumImportance)
		}
	}
	for _, model := range fixture.clydeState.models() {
		if model != testReviewModel {
			t.Fatalf("model = %q, want the configured %q", model, testReviewModel)
		}
	}

	// The whole label reaches telemetry, unparsed, which is its only use.
	accepted := fixture.logLinesContaining("webhook delivery accepted")
	logged := make([]string, 0, len(accepted))
	for _, record := range accepted {
		name, _ := record["label"].(string)
		logged = append(logged, name)
	}
	for _, label := range labels {
		if !slices.Contains(logged, label) {
			t.Fatalf("label %q was not logged for correlation, logged: %v", label, logged)
		}
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

func TestWebhookStartsReviewCheckBeforeReturningAccepted(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeBlockUntilCanceled: true,
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-immediate-check",
		body:       openedPayload(testDefectiveHead),
	})
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	if fixture.githubState.lastCheckStatus() != "in_progress" {
		t.Fatalf("check status = %q, want in_progress", fixture.githubState.lastCheckStatus())
	}
}

func TestEndToEndApprovesWithoutSevereFindings(t *testing.T) {
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

	fixture.waitForCheckConclusion(t, "success")
	if fixture.githubState.submitReviewCount() != 1 {
		t.Fatalf("submit review count = %d, want 1", fixture.githubState.submitReviewCount())
	}
	review := fixture.githubState.lastSubmitReview()
	if review["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("event = %v, want APPROVE", review["event"])
	}
	assertVerdictBody(t, review["body"], testDefectiveHead, false)
	comments, ok := review["comments"].([]any)
	if !ok || len(comments) != 0 {
		t.Fatalf("comments = %v, want none", review["comments"])
	}
	if fixture.githubState.completedCheckCount() != 1 {
		t.Fatalf("completed check count = %d, want 1", fixture.githubState.completedCheckCount())
	}
	if fixture.githubState.forbiddenEndpointHits() != 0 {
		t.Fatalf("forbidden endpoint hits = %d, want 0", fixture.githubState.forbiddenEndpointHits())
	}
}

// reviewBody returns the body of one submitted review.
func reviewBody(t *testing.T, review map[string]any) string {
	t.Helper()
	body, ok := review["body"].(string)
	if !ok {
		t.Fatalf("review body = %v, want string", review["body"])
	}
	return body
}

// One approving run published the identical Review block twice, two seconds
// apart: once as the summary comment and once as the approving review body.
// Both opened with the same heading and the same verdict sentence, so the
// reader saw two matching boxes stacked around the approval event.
//
// This counts across both surfaces rather than inspecting one, because the
// defect is that the pull request carries two of a thing it should carry one of.
func TestAnApprovingRunPublishesOneVisibleReviewBlock(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{approveReviewContent()},
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-one-box",
		body:       openedPayload(testDefectiveHead),
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	_ = response.Body.Close()

	fixture.waitForCheckConclusion(t, "success")
	fixture.waitForSubmitReviews(t, 1)

	review := fixture.githubState.lastSubmitReview()
	if review["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("event = %v, want APPROVE", review["event"])
	}
	comment := fixture.githubState.summaryCommentBody()
	verdict := reviewBody(t, review)

	headings := strings.Count(comment, "## Review") + strings.Count(verdict, "## Review")
	if headings != 1 {
		t.Fatalf("the pull request carries %d Review headings, want exactly 1\ncomment:\n%s\nverdict review:\n%s",
			headings, comment, verdict)
	}
	sentences := strings.Count(comment, "No severe findings.") +
		strings.Count(verdict, "No severe findings.")
	if sentences != 1 {
		t.Fatalf("the pull request states the verdict sentence %d times, want exactly 1\ncomment:\n%s\nverdict review:\n%s",
			sentences, comment, verdict)
	}
	// The one block that survives is the comment's, and the review still carries
	// the marker a later run reads to know this head was reviewed.
	if !strings.Contains(comment, "## Review") {
		t.Fatalf("summary comment = %q, want it to be the surviving Review block", comment)
	}
	assertVerdictBody(t, review["body"], testDefectiveHead, false)
}

// A blocking verdict keeps a body, because a block that names nothing to fix
// leaves no edit that could satisfy it. It still must not read as a copy of the
// summary comment.
func TestABlockingRunNamesItsReasonsWithoutRepeatingTheSummary(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{defectiveReviewContent(domain.Finding{
			Path:       testFindingPath,
			StartLine:  3,
			EndLine:    3,
			Title:      "Missing validation",
			Body:       "Validate the webhook payload before enqueue.",
			Importance: testMinimumImportance,
		})},
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-blocking-body",
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
	body := reviewBody(t, review)

	if !strings.Contains(body, "Waiting on:") {
		t.Fatalf("blocking verdict body names nothing to fix: %q", body)
	}
	if !strings.Contains(body, testFindingPath+":3") {
		t.Fatalf("blocking verdict body does not name the thread holding it: %q", body)
	}
	if strings.Contains(body, "<summary>Review details</summary>") {
		t.Fatalf("blocking verdict body repeats the comment's detail table: %q", body)
	}
	assertVerdictBody(t, review["body"], testDefectiveHead, true)
}

func TestEndToEndRequestChangesWithBlockingFinding(t *testing.T) {
	withIntegrationLock(t)
	blockingFinding := domain.Finding{
		Path:       testFindingPath,
		StartLine:  3,
		EndLine:    3,
		Title:      "Missing validation",
		Body:       "Validate the webhook payload before enqueue.",
		Importance: testMinimumImportance,
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
	if comments := fixture.githubState.streamedCommentBodies(); len(comments) != 1 {
		t.Fatalf("streamed comments = %d, want one inline comment", len(comments))
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
		Importance: testMinimumImportance,
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
	comments := fixture.githubState.streamedCommentBodies()
	if len(comments) != 1 {
		t.Fatalf("streamed comments = %d, want one inline comment", len(comments))
	}
	if comments[0]["start_line"] == nil {
		t.Fatalf("comment = %v, want a multiline range", comments[0])
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
		clydeResponses: []string{defectiveReviewContent(domain.Finding{
			Path: testFindingPath, StartLine: 3, EndLine: 3,
			Title: "Severe defect", Body: "Core behavior fails.", Importance: testMinimumImportance,
		})},
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

func TestShutdownCancelsActiveReviewAndCompletesCheck(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeBlockUntilCanceled: true,
	})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-shutdown",
		body:       openedPayload(testDefectiveHead),
	})
	_ = response.Body.Close()
	fixture.waitForClydeCalls(t, 1)
	fixture.githubState.setHead(testCorrectedHead)
	queued := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-shutdown-queued",
		body:       synchronizePayload(testCorrectedHead),
	})
	_ = queued.Body.Close()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := fixture.application.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	fixture.application = nil
	if fixture.githubState.lastCheckConclusion() != "failure" {
		t.Fatalf("check conclusion = %q, want failure", fixture.githubState.lastCheckConclusion())
	}
	if fixture.githubState.terminalCheckCount() != 2 {
		t.Fatalf("terminal checks = %d, want 2", fixture.githubState.terminalCheckCount())
	}
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
		fixture.waitForCheckConclusion(t, "success")
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

	fixture.waitForCheckConclusion(t, "success")
	if fixture.githubState.listedFilePages() < 2 {
		t.Fatalf("file page fetches = %d, want at least 2", fixture.githubState.listedFilePages())
	}
}

// The service keeps its one top level comment across a second run on a new
// head, updating it in place, while the reply endpoint stays untouched
// because nothing in this pipeline threads a reply onto another comment.
func TestEndToEndKeepsOneSummaryCommentAndNeverCallsReplyEndpoints(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{
		clydeResponses: []string{
			defectiveReviewContent(domain.Finding{
				Path:       testFindingPath,
				StartLine:  3,
				EndLine:    3,
				Title:      "Missing validation",
				Body:       "Validate the webhook payload before enqueue.",
				Importance: testMinimumImportance,
			}),
			defectiveReviewContent(domain.Finding{
				Path:       testFindingPath,
				StartLine:  4,
				EndLine:    4,
				Title:      "Unsafe fallback",
				Body:       "The new fallback still breaks core behavior.",
				Importance: testMinimumImportance,
			}),
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
	// The comment is rewritten after the review is submitted, and again at every
	// chunk checkpoint before it, so waiting on the review or on an update count
	// races the rewrite this test reads. Wait for the state it asserts.
	fixture.waitForSummaryHead(t, testCorrectedHead)
	// The second run's summary comment names the first run's head, so the
	// second run must have asked GitHub to compare that range rather than
	// listing the whole pull request again.
	if fixture.githubState.comparedRanges() < 1 {
		t.Fatalf("compare range fetches = %d, want at least 1", fixture.githubState.comparedRanges())
	}
	if fixture.githubState.listedFilePages() != 0 {
		t.Fatalf("full file list page fetches = %d, want 0: the second run must not list the whole pull request again",
			fixture.githubState.listedFilePages())
	}
	// Every verdict review states its decision. The old behavior submitted a
	// marker-only body once a summary review existed, and that body blocked a
	// live pull request while naming nothing to fix.
	assertVerdictBody(t, fixture.githubState.lastSubmitReview()["body"], testCorrectedHead, true)
	if fixture.githubState.forbiddenEndpointHits() != 0 {
		t.Fatalf("forbidden endpoint hits = %d, want 0", fixture.githubState.forbiddenEndpointHits())
	}
	if fixture.githubState.issueCommentCount() != 1 {
		t.Fatalf("issue comments = %d, want the one summary comment kept across both runs",
			fixture.githubState.issueCommentCount())
	}
	if fixture.githubState.issueCommentUpdateCount() < 1 {
		t.Fatalf("issue comment updates = %d, want the second run to update it in place",
			fixture.githubState.issueCommentUpdateCount())
	}
	summaryBody := fixture.githubState.summaryCommentBody()
	state, ok := marker.DecodeState(summaryBody)
	if !ok {
		t.Fatalf("summary comment body = %q, want a decodable state marker", summaryBody)
	}
	if state.LastReviewed != domain.HeadSHA(testCorrectedHead) {
		t.Fatalf("last reviewed = %q, want %q", state.LastReviewed, testCorrectedHead)
	}
	if state.Status != marker.StateDone {
		t.Fatalf("status = %q, want %q", state.Status, marker.StateDone)
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
		Importance: testMinimumImportance,
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
	if comments := fixture.githubState.streamedCommentBodies(); len(comments) != 1 {
		t.Fatalf("first streamed comments = %d, want one inline comment", len(comments))
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
	fixture.waitForCheckConclusion(t, "success")
	if fixture.githubState.submitReviewCount() != 2 {
		t.Fatalf("submit review count after fix = %d, want 2", fixture.githubState.submitReviewCount())
	}
	approval := fixture.githubState.lastSubmitReview()
	if approval["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("second event = %v, want APPROVE", approval["event"])
	}
	assertVerdictBody(t, approval["body"], testCorrectedHead, false)
	fixture.waitForResolveCalls(t, 1)
	if fixture.githubState.resolveCallCount() != 1 {
		t.Fatalf("resolve count = %d, want 1", fixture.githubState.resolveCallCount())
	}
	if fixture.githubState.lastResolveThreadID() != "thread-owned" {
		t.Fatalf("resolved thread = %q, want thread-owned", fixture.githubState.lastResolveThreadID())
	}
}

func TestWebhookAdmissionMintsOneRunIdentifierPerDelivery(t *testing.T) {
	withIntegrationLock(t)
	fixture := newAppFixture(t, appFixtureOptions{})
	defer fixture.close()

	response := fixture.postWebhook(t, webhookRequestOptions{
		eventType:  "pull_request",
		deliveryID: "delivery-run-id",
		body:       openedPayload(testDefectiveHead),
	})
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}

	logged := fixture.logLinesContaining("webhook delivery accepted")
	if len(logged) == 0 {
		t.Fatal("no accepted delivery line was logged")
	}
	if logged[0]["request_id"] != "delivery-run-id" {
		t.Fatalf("request_id = %v, want the delivery id", logged[0]["request_id"])
	}
	if logged[0]["trace_id"] == "" || logged[0]["trace_id"] == nil {
		t.Fatalf("trace_id = %v, want a minted trace id", logged[0]["trace_id"])
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
	clydeBlockUntilCanceled bool
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
	logs        *syncBuffer
}

// syncBuffer collects the JSON log lines the fixture's logger writes while
// the HTTP handler and background review workers run concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (buffer *syncBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buf.Write(data)
}

func (buffer *syncBuffer) lines() []string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return strings.Split(strings.TrimRight(buffer.buf.String(), "\n"), "\n")
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
		Port:          "0",
		ReviewWorkers: 4,
		ReviewModel:   testReviewModel,
		// Generous enough that no fixture diff, including the 101 file
		// pagination fixture, trips the admission gate under test here.
		ReviewMaxFiles:       1000,
		ReviewMaxChunks:      1000,
		ReviewChunkTimeout:   time.Minute,
		MinimumImportance:    testMinimumImportance,
		GitHubAppID:          12345,
		GitHubPrivateKey:     privateKey,                // gitleaks:allow
		GitHubWebhookSecret:  []byte(testWebhookSecret), // gitleaks:allow
		GitHubBotLogin:       testBotLogin,
		GitHubAPIBaseURL:     apiURL,
		GitHubGraphQLURL:     graphqlURL,
		ClydeBaseURL:         clydeURL,
		ClydeAPIKey:          "fixture-clyde-key", // gitleaks:allow
		CFAccessClientID:     "fixture-cf-id",     // gitleaks:allow
		CFAccessClientSecret: "fixture-cf-secret", // gitleaks:allow
	}

	// Wrapped in correlation.SlogHandler to match the production handler
	// chain in cmd/pr-review-agent/main.go, so tests can assert that a
	// captured log line carries the correlation identifiers a real run would.
	logs := &syncBuffer{}
	logger := slog.New(correlation.SlogHandler(
		slog.NewJSONHandler(logs, nil),
		correlation.HandlerOptions{},
	))
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
		logs:        logs,
	}
}

// logLinesContaining decodes every captured JSON log line and returns those
// whose msg field contains substring.
func (fixture *appFixture) logLinesContaining(substring string) []map[string]any {
	matches := make([]map[string]any, 0)
	for _, line := range fixture.logs.lines() {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		msg, _ := record["msg"].(string)
		if strings.Contains(msg, substring) {
			matches = append(matches, record)
		}
	}
	return matches
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
		blockUntilCanceled: options.clydeBlockUntilCanceled,
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

// waitForCheckCompletions waits until count runs have concluded their check.
// It counts completions rather than reading a conclusion value, so a test can
// wait for a specific run to finish even when it concludes the way the run
// before it did.
func (fixture *appFixture) waitForCheckCompletions(t *testing.T, count int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fixture.githubState.checkCompletions) >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("check completions = %d, want >= %d",
		atomic.LoadInt32(&fixture.githubState.checkCompletions), count)
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

// waitForSummaryHead waits until the durable state on the summary comment names
// the given commit, which is the last thing a completed run writes.
func (fixture *appFixture) waitForSummaryHead(t *testing.T, head string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, ok := marker.DecodeState(fixture.githubState.summaryCommentBody())
		if ok && state.LastReviewed == domain.HeadSHA(head) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("summary comment = %q, want the state to name %q",
		fixture.githubState.summaryCommentBody(), head)
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
	// settings is the review tuning header the worker attaches, sent verbatim so
	// a test can post one the service has to refuse to read.
	settings string
	// settingsSignature overrides the signature sent beside those values, so a
	// test can send a valid signature over something other than what it sent.
	settingsSignature string
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
	if options.settings != "" {
		request.Header.Set("X-Pr-Agent-Review-Settings", options.settings)
		settingsSignature := options.settingsSignature
		if settingsSignature == "" {
			settingsSignature = signReviewSettings(options.settings, options.body)
		}
		request.Header.Set("X-Pr-Agent-Review-Settings-Signature", settingsSignature)
	}

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

// signReviewSettings signs the tuning values the way the worker does: over the
// values and the body together, so a signature is worth nothing on any other
// delivery.
func signReviewSettings(settings string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(testWebhookSecret)) // gitleaks:allow
	_, _ = mac.Write([]byte(settings + "\n"))
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

// labeledPayload is a labeled delivery adding one label to the pull request.
// GitHub carries the label alongside the usual installation, repository, and
// pull request objects.
func labeledPayload(head string, labelName string) []byte {
	payload := map[string]any{
		"action": "labeled",
		"label": map[string]any{
			"name": labelName,
		},
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

func reviewThreadPayload(action string, head string) []byte {
	payload := map[string]any{
		"action": action,
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
	evidence := finding.Evidence
	if evidence == "" {
		// The line the defective fixture diff adds, so the finding passes the
		// evidence grounding gate the way an honest model answer does.
		evidence = "// missing validation"
	}
	payload := map[string]any{
		"coverage_complete": true,
		"findings": []map[string]any{{
			"path":       finding.Path,
			"start_line": finding.StartLine,
			"end_line":   finding.EndLine,
			"title":      finding.Title,
			"body":       finding.Body,
			"evidence":   evidence,
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
	return `{"coverage_complete":true,"findings":[]}`
}

func typographicReviewContent() string {
	return `{"summary":"Issue — details","coverage_complete":true,"findings":[{"path":"internal/app/handler.go","start_line":3,"end_line":3,"title":"Title – note","body":"Body — impact","evidence":"// missing validation","importance":9}]}`
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
					"author":     map[string]any{"login": testBotLogin},
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
	streamedComments     []map[string]any
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
	comparePageFetches   int32
	threadPageFetches    int32
	requests             int32
	issueComments        []map[string]any
	issueCommentUpdates  int32
	// checkCompletions counts every check run update that carried a conclusion.
	// A conclusion value alone cannot tell one run from the next, because the
	// second run of a pull request usually concludes the same way the first did,
	// so a test that waits on the value can read the earlier run's result and
	// assert against a run that has not finished.
	checkCompletions int32
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

func (state *githubServerState) terminalCheckCount() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	count := 0
	for _, item := range state.checkRuns {
		if item["conclusion"] != "" {
			count++
		}
	}
	return count
}

func (state *githubServerState) handleCreateReviewComment(
	writer http.ResponseWriter,
	request *http.Request,
) {
	body, err := readJSONBody(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	state.mu.Lock()
	state.streamedComments = append(state.streamedComments, body)
	count := len(state.streamedComments)
	// A posted comment starts a review thread, which is what the next run
	// reconciles against.
	threadID := "thread-owned"
	if len(state.threads) > 0 {
		threadID = fmt.Sprintf("thread-owned-%d", len(state.threads)+1)
	}
	state.threads = append(state.threads, map[string]any{
		"id":         threadID,
		"isResolved": false,
		"comments": map[string]any{
			"nodes": []map[string]any{{
				"databaseId": float64(count),
				"body":       body["body"],
				"path":       body["path"],
				"line":       body["line"],
				"startLine":  body["start_line"],
				"author":     map[string]any{"login": testBotLogin},
			}},
		},
	})
	state.mu.Unlock()
	writeJSON(writer, http.StatusCreated, map[string]any{
		"id":   float64(900 + count),
		"body": body["body"],
		"user": map[string]any{"login": testBotLogin},
	})
}

func (state *githubServerState) handleListIssueComments(writer http.ResponseWriter) {
	state.mu.Lock()
	defer state.mu.Unlock()
	writeJSON(writer, http.StatusOK, state.issueComments)
}

func (state *githubServerState) handleCreateIssueComment(writer http.ResponseWriter, request *http.Request) {
	body, err := readJSONBody(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	created := map[string]any{
		"id":   float64(2000 + len(state.issueComments)),
		"body": body["body"],
		"user": map[string]any{"login": testBotLogin},
	}
	state.issueComments = append(state.issueComments, created)
	writeJSON(writer, http.StatusCreated, created)
}

func (state *githubServerState) handleUpdateIssueComment(writer http.ResponseWriter, request *http.Request) {
	body, err := readJSONBody(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	idText := strings.TrimPrefix(request.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/comments/", testRepoOwner, testRepoName))
	var updated map[string]any
	for index, item := range state.issueComments {
		if fmt.Sprintf("%.0f", item["id"]) != idText {
			continue
		}
		item["body"] = body["body"]
		state.issueComments[index] = item
		updated = item
	}
	atomic.AddInt32(&state.issueCommentUpdates, 1)
	writeJSON(writer, http.StatusOK, updated)
}

// issueCommentCount reports how many top level comments the service has
// created, which must stay one across repeat runs.
func (state *githubServerState) issueCommentCount() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return len(state.issueComments)
}

func (state *githubServerState) issueCommentUpdateCount() int32 {
	return atomic.LoadInt32(&state.issueCommentUpdates)
}

// summaryCommentBody returns the body of the service's one top level
// comment, the durable state marker included.
func (state *githubServerState) summaryCommentBody() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.issueComments) == 0 {
		return ""
	}
	body, _ := state.issueComments[len(state.issueComments)-1]["body"].(string)
	return body
}

// streamedComments returns every inline comment that reached the pull request
// while the review ran.
func (state *githubServerState) streamedCommentBodies() []map[string]any {
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]map[string]any{}, state.streamedComments...)
}

func (state *githubServerState) lastSubmitReview() map[string]any {
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.submitReviews) == 0 {
		return nil
	}
	return state.submitReviews[len(state.submitReviews)-1]
}

func (state *githubServerState) summaryReviewBody() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, page := range state.listReviewPages {
		for _, item := range page {
			body, _ := item["body"].(string)
			if marker.HasSummary(body) {
				return body
			}
		}
	}
	return ""
}

func (state *githubServerState) summaryReviewCount() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	count := 0
	for _, page := range state.listReviewPages {
		for _, item := range page {
			body, _ := item["body"].(string)
			if marker.HasSummary(body) {
				count++
			}
		}
	}
	return count
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

func (state *githubServerState) lastCheckStatus() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.checkRuns) == 0 {
		return ""
	}
	status, _ := state.checkRuns[len(state.checkRuns)-1]["status"].(string)
	return status
}

func (state *githubServerState) listedFilePages() int32 {
	return atomic.LoadInt32(&state.filePageFetches)
}

func (state *githubServerState) comparedRanges() int32 {
	return atomic.LoadInt32(&state.comparePageFetches)
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

// markAllThreadsResolved is a person resolving every open thread in the GitHub
// UI: the threads stay listed, and later reads report them resolved.
func (state *githubServerState) markAllThreadsResolved() {
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, node := range state.threads {
		node["isResolved"] = true
	}
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

	// The service's one top level comment lives under the issue comments
	// endpoint, distinct from the inline review comments matched below.
	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/issues/") && strings.HasSuffix(request.URL.Path, "/comments") {
		state.handleListIssueComments(writer)
		return
	}

	if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/issues/") && strings.HasSuffix(request.URL.Path, "/comments") {
		state.handleCreateIssueComment(writer, request)
		return
	}

	if request.Method == http.MethodPatch && strings.Contains(request.URL.Path, "/issues/comments/") {
		state.handleUpdateIssueComment(writer, request)
		return
	}

	// Findings post one at a time as their chunks answer, so this endpoint
	// carries every inline comment a review produces.
	if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/comments") {
		state.handleCreateReviewComment(writer, request)
		return
	}

	if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/pulls/") && strings.HasSuffix(request.URL.Path, "/reviews") {
		state.handleSubmitReview(writer, request)
		return
	}

	if request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/pulls/") && strings.Contains(request.URL.Path, "/reviews/") {
		state.handleUpdateReview(writer, request)
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
		atomic.AddInt32(&state.comparePageFetches, 1)
		state.mu.Lock()
		files := state.compareFiles
		state.mu.Unlock()
		// GitHub always names the commit it measured the patches from. Here the
		// requested base is an ancestor of the head, so it is that base, which is
		// the case callers may map coordinates through.
		writeJSON(writer, http.StatusOK, map[string]any{
			"merge_base_commit": map[string]any{"sha": compareBaseFromPath(request.URL.Path)},
			"files":             files,
		})
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
	// Every created check run gets its own id, the way GitHub assigns one. Two
	// check runs sharing an id let a single update write to both, which both
	// hides that they are separate and counts one completion twice.
	createdID := state.nextCheckRunID
	state.nextCheckRunID++
	created := map[string]any{
		"id":         float64(createdID),
		"name":       body["name"],
		"head_sha":   body["head_sha"],
		"status":     body["status"],
		"conclusion": "",
		// The delivery that created this check run, which is how a redelivery
		// of that same delivery recognizes its own earlier admission.
		"external_id": body["external_id"],
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
	updatedID := float64(0)
	for index, item := range state.checkRuns {
		if fmt.Sprintf("%.0f", item["id"]) != checkID {
			continue
		}
		updatedID, _ = item["id"].(float64)
		if status, ok := body["status"].(string); ok && status != "" {
			item["status"] = status
		}
		if conclusion, ok := body["conclusion"].(string); ok {
			item["conclusion"] = conclusion
			if conclusion != "" {
				atomic.AddInt32(&state.checkCompletions, 1)
			}
		}
		state.checkRuns[index] = item
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"id":         updatedID,
		"name":       config.ReviewCheckName,
		"status":     body["status"],
		"conclusion": body["conclusion"],
	})
}

// githubReviewStateForEvent maps a submitted review event to the state GitHub
// reports for it afterwards: REQUEST_CHANGES is submitted and CHANGES_REQUESTED
// is read back, APPROVE comes back as APPROVED.
func githubReviewStateForEvent(event any) string {
	switch fmt.Sprint(event) {
	case string(domain.ReviewDecisionRequestChanges):
		return "CHANGES_REQUESTED"
	case string(domain.ReviewDecisionApprove):
		return "APPROVED"
	default:
		return "COMMENTED"
	}
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
	review := map[string]any{
		"id":        float64(100 + len(state.submitReviews)),
		"commit_id": commitID,
		"body":      reviewBody,
		"state":     githubReviewStateForEvent(body["event"]),
		"user":      map[string]any{"login": testBotLogin},
	}
	if len(state.listReviewPages) == 0 {
		state.listReviewPages = [][]map[string]any{{review}}
	} else {
		state.listReviewPages[0] = append(state.listReviewPages[0], review)
	}
	state.reviewPageIndex = 0
	state.mu.Unlock()

	writeJSON(writer, http.StatusOK, map[string]any{
		"id":        float64(42),
		"commit_id": body["commit_id"],
		"state":     body["event"],
		"body":      body["body"],
		"user":      map[string]any{"login": testBotLogin},
	})
}

func (state *githubServerState) handleUpdateReview(writer http.ResponseWriter, request *http.Request) {
	body, err := readJSONBody(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	reviewIDText := request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
	state.mu.Lock()
	defer state.mu.Unlock()
	for pageIndex, page := range state.listReviewPages {
		for reviewIndex, item := range page {
			if fmt.Sprintf("%.0f", item["id"]) != reviewIDText {
				continue
			}
			item["body"] = body["body"]
			state.listReviewPages[pageIndex][reviewIndex] = item
			writeJSON(writer, http.StatusOK, item)
			return
		}
	}
	writeJSON(writer, http.StatusNotFound, map[string]any{"message": "review not found"})
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
		// Resolving a thread on GitHub is durable: a later read of the same
		// thread reports it resolved. The verdict is computed from that read,
		// so a fixture that only counted the call would keep blocking on a
		// thread the run had already closed.
		for _, node := range state.threads {
			if node["id"] == threadID {
				node["isResolved"] = true
			}
		}
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

	// The nodes are copied under the lock because a concurrent resolve mutates
	// them in place. Encoding the shared maps after unlocking is a map read
	// racing a map write, which the runtime escalates to a crash.
	state.mu.Lock()
	threads := copyThreadNodes(state.threads)
	threadPageCount := len(state.threadPages)
	threadPageIndex := state.threadPageIndex
	var page []map[string]any
	if threadPageIndex < threadPageCount {
		page = copyThreadNodes(state.threadPages[threadPageIndex])
	}
	state.mu.Unlock()

	if threadPageCount > 0 {
		atomic.AddInt32(&state.threadPageFetches, 1)
		if threadPageIndex >= threadPageCount {
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
		state.mu.Lock()
		state.threadPageIndex++
		hasNext := state.threadPageIndex < threadPageCount
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

// compareBaseFromPath reads the base commit out of a compare request path,
// which looks like /repos/owner/repo/compare/{base}...{head}.
func compareBaseFromPath(path string) string {
	_, after, found := strings.Cut(path, "/compare/")
	if !found {
		return ""
	}
	base, _, found := strings.Cut(after, "...")
	if !found {
		return ""
	}
	return base
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
	mu sync.Mutex
	// requestedModels is the model named on each request, which is the only
	// place a changed model name would show up.
	requestedModels    []string
	responses          []string
	reconcileResponses []string
	index              int
	reconcileIndex     int
	status             int
	reconcileStatus    int
	requests           int32
	reconcileRequests  int32
	blockUntilCanceled bool
}

func (state *clydeServerState) requestCount() int32 {
	return atomic.LoadInt32(&state.requests)
}

func (state *clydeServerState) reconcileRequestCount() int32 {
	return atomic.LoadInt32(&state.reconcileRequests)
}

// models returns the model named on every request this server has answered.
func (state *clydeServerState) models() []string {
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]string{}, state.requestedModels...)
}

func (state *clydeServerState) handle(writer http.ResponseWriter, request *http.Request) {
	atomic.AddInt32(&state.requests, 1)

	body, err := readJSONBody(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if state.blockUntilCanceled {
		<-request.Context().Done()
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
	if model, ok := body["model"].(string); ok {
		state.requestedModels = append(state.requestedModels, model)
	}
	status := state.status
	if isReconcile && state.reconcileStatus != 0 {
		status = state.reconcileStatus
	}
	failed := status != 0 && status != http.StatusOK
	var content string
	// A failing endpoint serves no content, and a fixture configured with only
	// a status has no responses to index.
	if !failed {
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
	}
	state.mu.Unlock()

	if failed {
		http.Error(writer, "clyde request failed", status)
		return
	}

	writeCompletionStream(writer, content)
}

func failForbiddenEndpoint(writer http.ResponseWriter, request *http.Request) bool {
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

// summaryProse is the wording that belongs to the one top level comment. A
// verdict review body carrying any of it puts a second identical Review box on
// the page, which is what a reader saw twice two seconds apart.
var summaryProse = []string{
	"## Review",
	"No severe findings.",
	"Severe findings are listed inline.",
	"<summary>Review details</summary>",
}

// assertVerdictBody checks one verdict review body against the rule that it
// never restates the summary comment, and keeps the review marker the service
// reads back to recognize a head it already reviewed.
//
// An approving verdict is the marker alone, because the approval event is the
// message and everything else is in the comment. A blocking one adds its
// decision and what it waits on, because a block naming nothing to fix leaves
// no edit that could satisfy it.
func assertVerdictBody(t *testing.T, value any, head string, blocking bool) {
	t.Helper()
	body, ok := value.(string)
	if !ok {
		t.Fatalf("body = %v, want string", value)
	}
	for _, prose := range summaryProse {
		if strings.Contains(body, prose) {
			t.Fatalf("verdict body repeats the summary comment's %q, so the reader sees two Review boxes: %q",
				prose, body)
		}
	}
	if markerHead, found := marker.FindReview(body); !found || markerHead != domain.HeadSHA(head) {
		t.Fatalf("body = %q, want the review marker for %s", body, head)
	}
	if !blocking {
		if strings.TrimSpace(body) != marker.Review(domain.HeadSHA(head)) {
			t.Fatalf("approving verdict body carries visible prose beside the marker: %q", body)
		}
		return
	}
	if !strings.Contains(body, "Changes requested.") {
		t.Fatalf("blocking verdict body does not state its decision: %q", body)
	}
	if !strings.Contains(body, "Waiting on:") {
		t.Fatalf("blocking verdict body names nothing to fix, so no edit can satisfy it: %q", body)
	}
}

func writeCompletionStream(writer http.ResponseWriter, content string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	chunks := []map[string]any{
		{
			"id":      "chatcmpl-test",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   testReviewModel,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": content},
			}},
		},
		{
			"id":      "chatcmpl-test",
			"object":  "chat.completion.chunk",
			"created": 0,
			"model":   testReviewModel,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
		},
	}
	for _, chunk := range chunks {
		encoded, _ := json.Marshal(chunk)
		_, _ = writer.Write([]byte("data: " + string(encoded) + "\n\n"))
	}
	_, _ = writer.Write([]byte("data: [DONE]\n\n"))
}

// copyThreadNodes deep copies thread nodes one level down, which is the level
// the resolve mutation writes. The nested comment nodes are never mutated
// after construction, so sharing them is safe.
func copyThreadNodes(nodes []map[string]any) []map[string]any {
	copied := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		fresh := make(map[string]any, len(node))
		for key, value := range node {
			fresh[key] = value
		}
		copied = append(copied, fresh)
	}
	return copied
}

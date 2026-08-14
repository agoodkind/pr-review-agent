package openai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/review"
)

func testAPIKeyValue() string {
	return "fixture-openai-" + strings.Repeat("k", 12)
}

func testCFClientIDValue() string {
	return "fixture-cf-id"
}

func testCFClientSecretValue() string {
	return "fixture-cf-" + strings.Repeat("s", 12)
}

func TestReviewSendsExactModelHeadersPolicyAndSchema(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.completionContent = validReviewContent()

	_, err := client.Review(context.Background(), "review input")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if state.requestCount != 1 {
		t.Fatalf("request count = %d, want 1", state.requestCount)
	}

	request := state.lastRequest
	if request.Header.Get("Authorization") != "Bearer "+testAPIKeyValue() {
		t.Fatalf("Authorization = %q, want Bearer prefix with api key", request.Header.Get("Authorization"))
	}
	if request.Header.Get("Cf-Access-Client-Id") != testCFClientIDValue() {
		t.Fatalf("Cf-Access-Client-Id = %q, want %q", request.Header.Get("Cf-Access-Client-Id"), testCFClientIDValue())
	}
	if request.Header.Get("Cf-Access-Client-Secret") != testCFClientSecretValue() {
		t.Fatalf("Cf-Access-Client-Secret mismatch")
	}

	body := state.lastRequestBody
	if body["model"] != config.Model {
		t.Fatalf("model = %v, want %q", body["model"], config.Model)
	}
	if body["reasoning_effort"] != config.ReasoningEffort {
		t.Fatalf("reasoning_effort = %v, want %q", body["reasoning_effort"], config.ReasoningEffort)
	}
	if body["max_completion_tokens"] != float64(config.MaximumOutputTokens) {
		t.Fatalf("max_completion_tokens = %v, want %d", body["max_completion_tokens"], config.MaximumOutputTokens)
	}

	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %T len=%d, want 2 chat messages", body["messages"], len(messages))
	}
	systemMessage, ok := messages[0].(map[string]any)
	if !ok {
		t.Fatalf("first message = %v, want system role", messages[0])
	}
	systemContent, _ := systemMessage["content"].(string)
	if !strings.Contains(systemContent, config.WritingPolicy) {
		t.Fatalf("system message missing writing policy")
	}
	if !strings.Contains(systemContent, review.UntrustedInputPolicy) {
		t.Fatalf("system message missing untrusted input policy")
	}
	if !strings.Contains(systemContent, "Return only JSON") {
		t.Fatalf("system message missing JSON-only fallback")
	}
	if !strings.Contains(systemContent, `"coverage_complete"`) {
		t.Fatalf("system message missing review schema fallback")
	}

	responseFormat, ok := body["response_format"].(map[string]any)
	if !ok || responseFormat["type"] != "json_schema" {
		t.Fatalf("response_format = %v, want json_schema type", body["response_format"])
	}
	jsonSchema, ok := responseFormat["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("json_schema missing")
	}
	if jsonSchema["strict"] != true {
		t.Fatalf("strict = %v, want true", jsonSchema["strict"])
	}
	if jsonSchema["name"] != "review_result" {
		t.Fatalf("schema name = %v, want review_result", jsonSchema["name"])
	}
}

func TestReviewRejectsInvalidFindings(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.completionContent = `{"summary":"ok","coverage_complete":true,"findings":[{"path":"a.go","start_line":1,"end_line":1,"title":"t","body":"b","importance":0}]}`
	_, err := client.Review(context.Background(), "prompt")
	if err == nil {
		t.Fatal("Review with invalid importance: want error")
	}
	if state.requestCount != 1 {
		t.Fatalf("request count = %d, want 1 without retry", state.requestCount)
	}
}

func TestReviewRetriesTransientFailures(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.statusSequence = []int{
		http.StatusInternalServerError,
		http.StatusTooManyRequests,
		http.StatusOK,
	}
	state.completionContent = validReviewContent()

	_, err := client.Review(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if state.requestCount != 3 {
		t.Fatalf("request count = %d, want 3", state.requestCount)
	}
}

func TestReviewDoesNotRetryAuthenticationFailure(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.statusSequence = []int{http.StatusUnauthorized}

	_, err := client.Review(context.Background(), "prompt")
	if err == nil {
		t.Fatal("Review: want authentication error")
	}
	if state.requestCount != 1 {
		t.Fatalf("request count = %d, want 1", state.requestCount)
	}
}

func TestReconcileAcceptsOnlyKnownResolutionValues(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.completionContent = `{"resolutions":[{"thread_node_id":"thread-1","resolution":"resolved","reason":"fixed"},{"thread_node_id":"thread-2","resolution":"open","reason":"still bad"},{"thread_node_id":"thread-3","resolution":"uncertain","reason":"unclear"}]}`

	resolutions, err := client.Reconcile(context.Background(), "reconcile input")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(resolutions) != 3 {
		t.Fatalf("resolution count = %d, want 3", len(resolutions))
	}
	if resolutions[0].Resolution != domain.ResolutionResolved {
		t.Fatalf("first resolution = %q, want resolved", resolutions[0].Resolution)
	}
}

func TestReconcileRejectsDuplicateThreadIDs(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.completionContent = `{"resolutions":[{"thread_node_id":"thread-1","resolution":"resolved","reason":"one"},{"thread_node_id":"thread-1","resolution":"open","reason":"two"}]}`

	_, err := client.Reconcile(context.Background(), "reconcile input")
	if err == nil {
		t.Fatal("Reconcile duplicate thread ids: want error")
	}
	if state.requestCount != 1 {
		t.Fatalf("request count = %d, want 1 without retry", state.requestCount)
	}
}

type testServerState struct {
	requestCount      int32
	lastRequest       *http.Request
	lastRequestBody   map[string]any
	statusSequence    []int
	statusIndex       int
	completionContent string
}

func newTestClient(t *testing.T) (*openai.Client, *httptest.Server, *testServerState) {
	t.Helper()

	state := &testServerState{
		completionContent: validReviewContent(),
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&state.requestCount, 1)
		state.lastRequest = request
		body, err := readJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		state.lastRequestBody = body

		status := http.StatusOK
		if state.statusIndex < len(state.statusSequence) {
			status = state.statusSequence[state.statusIndex]
			state.statusIndex++
		}

		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			writer.Header().Set("Retry-After-Ms", "0")
			writeJSON(writer, status, map[string]any{
				"error": map[string]any{
					"message": "request failed",
					"type":    "invalid_request_error",
					"param":   "",
					"code":    "request_failed",
				},
			})
			return
		}

		writeJSON(writer, status, map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": state.completionContent}}},
		})
	}))

	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse base URL: %v", err)
	}

	cfg := config.Config{
		ClydeBaseURL:         baseURL,
		ClydeAPIKey:          testAPIKeyValue(),
		CFAccessClientID:     testCFClientIDValue(),
		CFAccessClientSecret: testCFClientSecretValue(), // gitleaks:allow
	}
	return openai.NewClient(cfg, server.Client()), server, state
}

func validReviewContent() string {
	return `{"summary":"No issues found.","coverage_complete":true,"findings":[]}`
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

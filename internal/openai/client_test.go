package openai_test

import (
	"context"
	"encoding/json"
	"errors"
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

const (
	testPrimaryModel  = "fixture-primary-model"
	testFallbackModel = "fixture-fallback-model"
)

func testAPIKeyValue() string {
	return "fixture-openai-" + strings.Repeat("k", 12)
}

func testFallbackAPIKeyValue() string {
	return "fixture-fallback-" + strings.Repeat("f", 12)
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
	if body["model"] != testPrimaryModel {
		t.Fatalf("model = %v, want %q", body["model"], testPrimaryModel)
	}
	if body["reasoning_effort"] != config.ReasoningEffort {
		t.Fatalf("reasoning_effort = %v, want %q", body["reasoning_effort"], config.ReasoningEffort)
	}
	if body["max_completion_tokens"] != float64(config.MaximumOutputTokens) {
		t.Fatalf("max_completion_tokens = %v, want %d", body["max_completion_tokens"], config.MaximumOutputTokens)
	}
	if body["stream"] != true {
		t.Fatalf("stream = %v, want true", body["stream"])
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
	if !strings.Contains(systemContent, "importance 7 or higher") {
		t.Fatalf("system message missing configured importance")
	}
	if strings.Contains(systemContent, "security breach") {
		t.Fatalf("system message restricts configurable importance to fixed categories")
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
	schema, ok := jsonSchema["schema"].(map[string]any)
	if !ok {
		t.Fatalf("schema = %v, want object", jsonSchema["schema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %v, want object", schema["properties"])
	}
	findings, ok := properties["findings"].(map[string]any)
	if !ok {
		t.Fatalf("findings schema = %v, want object", properties["findings"])
	}
	items, ok := findings["items"].(map[string]any)
	if !ok {
		t.Fatalf("finding items = %v, want object", findings["items"])
	}
	findingProperties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("finding properties = %v, want object", items["properties"])
	}
	suggestion, ok := findingProperties["suggestion"].(map[string]any)
	if !ok || suggestion["type"] != "string" {
		t.Fatalf("suggestion schema = %v, want string", findingProperties["suggestion"])
	}
	required, ok := items["required"].([]any)
	if !ok {
		t.Fatalf("finding required = %v, want array", items["required"])
	}
	suggestionRequired := false
	for _, field := range required {
		if field == "suggestion" {
			suggestionRequired = true
			break
		}
	}
	if !suggestionRequired {
		t.Fatalf("finding required = %v, want suggestion", required)
	}
}

func TestReviewRejectsInvalidFindings(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.completionContent = `{"coverage_complete":true,"findings":[{"path":"a.go","start_line":1,"end_line":1,"title":"t","body":"b","importance":0}]}`
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
	for _, detail := range []string{"HTTP 401 Unauthorized", "request_failed", "request failed"} {
		if !strings.Contains(err.Error(), detail) {
			t.Fatalf("error = %q, want %q", err, detail)
		}
	}
	if state.requestCount != 1 {
		t.Fatalf("request count = %d, want 1", state.requestCount)
	}
}

func TestReviewReportsGatewayFoldedUpstreamMessage(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.statusSequence = []int{http.StatusBadRequest}
	state.errorPayload = map[string]any{
		"message": "upstream call failed: provider=codex upstream_status=429 " +
			"upstream_code=usage_limit_reached upstream_message=You have used all included usage.",
		"type":  "invalid_request_error",
		"param": "",
		"code":  "upstream_failed",
	}

	_, err := client.Review(context.Background(), "prompt")
	if err == nil {
		t.Fatal("Review: want provider error")
	}
	if !strings.Contains(err.Error(), "You have used all included usage.") {
		t.Fatalf("error = %q, want the folded upstream message", err)
	}

	var providerError *openai.ProviderError
	if !errors.As(err, &providerError) {
		t.Fatalf("error = %q, want a *openai.ProviderError", err)
	}
	if !providerError.UsageExceeded() {
		t.Fatalf("UsageExceeded() = false for %q, want true", err)
	}
}

func TestProviderErrorClassifiesUsageExhaustion(t *testing.T) {
	cases := []struct {
		name          string
		providerError openai.ProviderError
		want          bool
	}{
		{
			name:          "openai insufficient quota",
			providerError: openai.ProviderError{StatusCode: 429, Type: "insufficient_quota", Code: "insufficient_quota"},
			want:          true,
		},
		{
			name: "gateway folded usage limit",
			providerError: openai.ProviderError{
				StatusCode: 400,
				Type:       "invalid_request_error",
				Code:       "upstream_failed",
				Message:    "upstream call failed: upstream_code=usage_limit_reached",
			},
			want: true,
		},
		{
			name:          "payment required",
			providerError: openai.ProviderError{StatusCode: 402, Code: "upstream_failed"},
			want:          true,
		},
		{
			name: "plain rate limit",
			providerError: openai.ProviderError{
				StatusCode: 429,
				Type:       "rate_limit_error",
				Code:       "rate_limit_exceeded",
				Message:    "Please retry shortly.",
			},
			want: false,
		},
		{
			name: "schema violation",
			providerError: openai.ProviderError{
				StatusCode: 400,
				Type:       "invalid_request_error",
				Code:       "upstream_malformed_request",
				Message:    "missing_required_parameter",
			},
			want: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.providerError.UsageExceeded(); got != testCase.want {
				t.Fatalf("UsageExceeded() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestReviewRejectsUnterminatedStream(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.streamResponse = func(writer http.ResponseWriter) {
		writeStreamFrames(writer, []map[string]any{
			completionStreamChunk(validReviewContent(), ""),
		}, false)
	}

	_, err := client.Review(context.Background(), "prompt")
	if err == nil || !strings.Contains(err.Error(), "ended without a finish reason") {
		t.Fatalf("Review error = %v, want missing finish reason", err)
	}
}

func TestReviewRejectsIncompleteFinishReason(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.streamResponse = func(writer http.ResponseWriter) {
		writeStreamFrames(writer, []map[string]any{
			completionStreamChunk(validReviewContent(), "length"),
		}, true)
	}

	_, err := client.Review(context.Background(), "prompt")
	if err == nil || !strings.Contains(err.Error(), "finish reason length") {
		t.Fatalf("Review error = %v, want length finish reason", err)
	}
}

func TestReviewRejectsMalformedStream(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.streamResponse = func(writer http.ResponseWriter) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("data: {\n\n"))
	}

	if _, err := client.Review(context.Background(), "prompt"); err == nil {
		t.Fatal("Review malformed stream: want error")
	}
}

func TestReviewRejectsProviderStreamError(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.streamResponse = func(writer http.ResponseWriter) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("data: {\"error\":{\"message\":\"upstream failed\"}}\n\n"))
	}

	if _, err := client.Review(context.Background(), "prompt"); err == nil {
		t.Fatal("Review provider stream error: want error")
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

func TestFallbackStaysUnusedWhenThePrimaryAnswers(t *testing.T) {
	fixture := newFallbackTestClient(t, false)

	if _, err := fixture.client.Review(context.Background(), "prompt"); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if fixture.primary.requestCount != 1 {
		t.Fatalf("primary request count = %d, want 1", fixture.primary.requestCount)
	}
	if fixture.fallback.requestCount != 0 {
		t.Fatalf("fallback request count = %d, want 0", fixture.fallback.requestCount)
	}
}

func TestFallbackAnswersWhenThePrimaryReportsExhaustedUsage(t *testing.T) {
	fixture := newFallbackTestClient(t, false)
	fixture.primary.statusSequence = []int{http.StatusBadRequest}
	fixture.primary.errorPayload = gatewayUsageExceededPayload()

	result, err := fixture.client.Review(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !result.CoverageComplete {
		t.Fatalf("result = %+v, want the fallback result", result)
	}
	if fixture.fallback.requestCount != 1 {
		t.Fatalf("fallback request count = %d, want 1", fixture.fallback.requestCount)
	}

	body := fixture.fallback.lastRequestBody
	if body["model"] != testFallbackModel {
		t.Fatalf("fallback model = %v, want %q", body["model"], testFallbackModel)
	}
	authorization := fixture.fallback.lastRequest.Header.Get("Authorization")
	if authorization != "Bearer "+testFallbackAPIKeyValue() {
		t.Fatalf("fallback Authorization = %q, want the fallback key", authorization)
	}
	if header := fixture.fallback.lastRequest.Header.Get("Cf-Access-Client-Id"); header != "" {
		t.Fatalf("Cf-Access-Client-Id = %q, want none for a public endpoint", header)
	}
}

func TestFallbackSendsAccessHeadersOnlyWhenConfigured(t *testing.T) {
	fixture := newFallbackTestClient(t, true)
	fixture.primary.statusSequence = []int{http.StatusBadRequest}
	fixture.primary.errorPayload = gatewayUsageExceededPayload()

	if _, err := fixture.client.Review(context.Background(), "prompt"); err != nil {
		t.Fatalf("Review: %v", err)
	}
	header := fixture.fallback.lastRequest.Header.Get("Cf-Access-Client-Id")
	if header != testCFClientIDValue() {
		t.Fatalf("Cf-Access-Client-Id = %q, want %q", header, testCFClientIDValue())
	}
	if fixture.fallback.lastRequest.Header.Get("Cf-Access-Client-Secret") != testCFClientSecretValue() {
		t.Fatal("fallback Cf-Access-Client-Secret mismatch")
	}
}

func TestReconcileUsesTheFallbackToo(t *testing.T) {
	fixture := newFallbackTestClient(t, false)
	fixture.primary.statusSequence = []int{http.StatusBadRequest}
	fixture.primary.errorPayload = gatewayUsageExceededPayload()
	fixture.fallback.completionContent = `{"resolutions":[]}`

	if _, err := fixture.client.Reconcile(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if fixture.fallback.requestCount != 1 {
		t.Fatalf("fallback request count = %d, want 1", fixture.fallback.requestCount)
	}
}

func TestFallbackStaysUnusedForAFailureThatIsNotExhaustedUsage(t *testing.T) {
	fixture := newFallbackTestClient(t, false)
	fixture.primary.statusSequence = []int{http.StatusBadRequest}
	fixture.primary.errorPayload = map[string]any{
		"message": "missing_required_parameter",
		"type":    "invalid_request_error",
		"param":   "",
		"code":    "upstream_malformed_request",
	}

	_, err := fixture.client.Review(context.Background(), "prompt")
	if err == nil {
		t.Fatal("Review: want the primary error")
	}
	if !strings.Contains(err.Error(), "upstream_malformed_request") {
		t.Fatalf("err = %q, want the primary cause", err)
	}
	if fixture.fallback.requestCount != 0 {
		t.Fatalf("fallback request count = %d, want 0", fixture.fallback.requestCount)
	}
}

func TestBothProvidersFailingReportsBothCauses(t *testing.T) {
	fixture := newFallbackTestClient(t, false)
	fixture.primary.statusSequence = []int{http.StatusBadRequest}
	fixture.primary.errorPayload = gatewayUsageExceededPayload()
	fixture.fallback.statusSequence = []int{http.StatusUnauthorized}

	_, err := fixture.client.Review(context.Background(), "prompt")
	if err == nil {
		t.Fatal("Review: want an error")
	}
	if !strings.Contains(err.Error(), "The usage limit has been reached") {
		t.Fatalf("err = %q, want the primary cause", err)
	}
	if !strings.Contains(err.Error(), "HTTP 401 Unauthorized") {
		t.Fatalf("err = %q, want the fallback cause", err)
	}

	var providerError *openai.ProviderError
	if !errors.As(err, &providerError) {
		t.Fatalf("err = %q, want a *openai.ProviderError in the chain", err)
	}
	if !providerError.UsageExceeded() {
		t.Fatalf("err = %q, want the usage classifier to still match", err)
	}
}

func TestPrimaryErrorIsUnchangedWithoutAFallback(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.statusSequence = []int{http.StatusBadRequest}
	state.errorPayload = gatewayUsageExceededPayload()

	_, err := client.Review(context.Background(), "prompt")
	if err == nil {
		t.Fatal("Review: want an error")
	}
	want := "model provider returned HTTP 400 Bad Request: invalid_request_error: upstream_failed: " +
		"scan codex SSE events: The usage limit has been reached " +
		"(Clyde request_id=chatcmpl-c6d74129dc85c5f42cd0a490)"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
	if state.requestCount != 1 {
		t.Fatalf("request count = %d, want 1", state.requestCount)
	}
}

type testServerState struct {
	requestCount      int32
	lastRequest       *http.Request
	lastRequestBody   map[string]any
	statusSequence    []int
	statusIndex       int
	completionContent string
	streamResponse    func(http.ResponseWriter)
	errorPayload      map[string]any
}

func newTestClient(t *testing.T) (*openai.Client, *httptest.Server, *testServerState) {
	t.Helper()

	state := &testServerState{completionContent: validReviewContent()}
	server := newProviderServer(state)
	cfg := config.Config{
		MinimumImportance:    7,
		ReviewModel:          testPrimaryModel,
		ClydeBaseURL:         mustParseURL(t, server.URL),
		ClydeAPIKey:          testAPIKeyValue(),
		CFAccessClientID:     testCFClientIDValue(),
		CFAccessClientSecret: testCFClientSecretValue(), // gitleaks:allow
	}
	return openai.NewClient(cfg, server.Client()), server, state
}

// fallbackFixture holds one client wired to a primary and a fallback provider,
// so a test can assert which endpoint received the request.
type fallbackFixture struct {
	client   *openai.Client
	primary  *testServerState
	fallback *testServerState
}

func newFallbackTestClient(t *testing.T, withAccessHeaders bool) *fallbackFixture {
	t.Helper()

	primaryState := &testServerState{completionContent: validReviewContent()}
	fallbackState := &testServerState{completionContent: validReviewContent()}
	primaryServer := newProviderServer(primaryState)
	t.Cleanup(primaryServer.Close)
	fallbackServer := newProviderServer(fallbackState)
	t.Cleanup(fallbackServer.Close)

	cfg := config.Config{
		MinimumImportance:       7,
		ReviewModel:             testPrimaryModel,
		ClydeBaseURL:            mustParseURL(t, primaryServer.URL),
		ClydeAPIKey:             testAPIKeyValue(),
		CFAccessClientID:        testCFClientIDValue(),
		CFAccessClientSecret:    testCFClientSecretValue(), // gitleaks:allow
		FallbackBaseURL:         mustParseURL(t, fallbackServer.URL),
		FallbackModel:           testFallbackModel,
		FallbackAPIKey:          testFallbackAPIKeyValue(), // gitleaks:allow
		FallbackOnUsageExceeded: true,
	}
	if withAccessHeaders {
		cfg.FallbackCFAccessClientID = testCFClientIDValue()
		cfg.FallbackCFAccessClientSecret = testCFClientSecretValue() // gitleaks:allow
	}

	return &fallbackFixture{
		client:   openai.NewClient(cfg, primaryServer.Client()),
		primary:  primaryState,
		fallback: fallbackState,
	}
}

func mustParseURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatalf("Parse URL %q: %v", value, err)
	}
	return parsed
}

func newProviderServer(state *testServerState) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
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
			payload := state.errorPayload
			if payload == nil {
				payload = map[string]any{
					"message": "request failed",
					"type":    "invalid_request_error",
					"param":   "",
					"code":    "request_failed",
				}
			}
			writeJSON(writer, status, map[string]any{"error": payload})
			return
		}
		if state.streamResponse != nil {
			state.streamResponse(writer)
			return
		}
		if body["stream"] == true {
			writeStream(writer, state.completionContent)
			return
		}

		writeJSON(writer, status, map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": state.completionContent}}},
		})
	}))
}

// gatewayUsageExceededPayload is the error body the deployed gateway returned
// when the primary provider had no remaining usage.
func gatewayUsageExceededPayload() map[string]any {
	return map[string]any{
		"message": "scan codex SSE events: The usage limit has been reached " +
			"(Clyde request_id=chatcmpl-c6d74129dc85c5f42cd0a490)",
		"type":  "invalid_request_error",
		"param": "",
		"code":  "upstream_failed",
	}
}

func validReviewContent() string {
	return `{"coverage_complete":true,"findings":[]}`
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func writeStream(writer http.ResponseWriter, content string) {
	writeStreamFrames(writer, []map[string]any{
		completionStreamChunk(content, ""),
		completionStreamChunk("", "stop"),
	}, true)
}

func completionStreamChunk(content string, finishReason string) map[string]any {
	choice := map[string]any{
		"index": 0,
		"delta": map[string]any{"content": content},
	}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}
	return map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   testPrimaryModel,
		"choices": []map[string]any{choice},
	}
}

func writeStreamFrames(writer http.ResponseWriter, chunks []map[string]any, done bool) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	for _, chunk := range chunks {
		encoded, _ := json.Marshal(chunk)
		_, _ = writer.Write([]byte("data: " + string(encoded) + "\n\n"))
	}
	if done {
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}
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

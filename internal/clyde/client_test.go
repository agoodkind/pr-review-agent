package clyde_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/clyde"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
)

func testAPIKeyValue() string {
	return "fixture-clyde-" + strings.Repeat("k", 12)
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
	if request.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", request.Header.Get("Content-Type"))
	}
	if request.Header.Get("CF-Access-Client-ID") != testCFClientIDValue() {
		t.Fatalf("CF-Access-Client-ID = %q, want %q", request.Header.Get("CF-Access-Client-ID"), testCFClientIDValue())
	}
	if request.Header.Get("CF-Access-Client-Secret") != testCFClientSecretValue() {
		t.Fatalf("CF-Access-Client-Secret mismatch")
	}

	body := state.lastRequestBody
	if body == nil {
		t.Fatal("request body missing")
	}
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
	if !ok || systemMessage["role"] != "system" {
		t.Fatalf("first message = %v, want system role", messages[0])
	}
	systemContent, _ := systemMessage["content"].(string)
	if !strings.Contains(systemContent, config.WritingPolicy) {
		t.Fatalf("system message missing writing policy")
	}
	if !strings.Contains(systemContent, clyde.UntrustedInputPolicy) {
		t.Fatalf("system message missing untrusted input policy")
	}

	userMessage, ok := messages[1].(map[string]any)
	if !ok || userMessage["role"] != "user" || userMessage["content"] != "review input" {
		t.Fatalf("user message = %v, want review input", messages[1])
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
		t.Fatalf("schema body missing")
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("root additionalProperties = %v, want false", schema["additionalProperties"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties missing")
	}
	findings, ok := properties["findings"].(map[string]any)
	if !ok {
		t.Fatalf("findings schema missing")
	}
	findingItems, ok := findings["items"].(map[string]any)
	if !ok || findingItems["additionalProperties"] != false {
		t.Fatalf("finding item additionalProperties = %v, want false", findingItems["additionalProperties"])
	}
}

func TestReviewRejectsUnknownFieldsAndInvalidFindings(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.completionContent = `{"summary":"ok","coverage_complete":true,"findings":[{"path":"a.go","start_line":1,"end_line":1,"title":"t","body":"b","importance":1,"extra":true}]}`
	_, err := client.Review(context.Background(), "prompt")
	if err == nil {
		t.Fatal("Review with unknown finding field: want error")
	}
	if state.requestCount != 1 {
		t.Fatalf("request count = %d, want 1 without retry", state.requestCount)
	}

	state.completionContent = `{"summary":"ok","coverage_complete":true,"findings":[{"path":"a.go","start_line":1,"end_line":1,"title":"t","body":"b","importance":0}]}`
	_, err = client.Review(context.Background(), "prompt")
	if err == nil {
		t.Fatal("Review with invalid importance: want error")
	}
	if state.requestCount != 2 {
		t.Fatalf("request count = %d, want 2 without retry", state.requestCount)
	}
}

func TestReviewRetriesTransientFailuresThreeTimes(t *testing.T) {
	sleepCalls := 0
	client, server, state := newTestClientWithSleep(t, func(context.Context, time.Duration) error {
		sleepCalls++
		return nil
	})
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
	if sleepCalls != 2 {
		t.Fatalf("sleep calls = %d, want 2", sleepCalls)
	}
}

func TestReviewDoesNotRetryAuthenticationFailure(t *testing.T) {
	sleepCalls := 0
	client, server, state := newTestClientWithSleep(t, func(context.Context, time.Duration) error {
		sleepCalls++
		return nil
	})
	defer server.Close()

	state.statusSequence = []int{http.StatusUnauthorized}
	state.completionContent = validReviewContent()

	_, err := client.Review(context.Background(), "prompt")
	if err == nil {
		t.Fatal("Review: want authentication error")
	}
	var apiErr clyde.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", apiErr.StatusCode)
	}
	if state.requestCount != 1 {
		t.Fatalf("request count = %d, want 1", state.requestCount)
	}
	if sleepCalls != 0 {
		t.Fatalf("sleep calls = %d, want 0", sleepCalls)
	}
}

func TestReviewErrorsDoNotContainCredentials(t *testing.T) {
	client, server, state := newTestClient(t)
	defer server.Close()

	state.statusSequence = []int{http.StatusUnauthorized}
	apiKey := testAPIKeyValue()
	cfSecret := testCFClientSecretValue()
	state.errorBody = fmt.Sprintf("invalid key Bearer %s and secret %s", apiKey, cfSecret)

	_, err := client.Review(context.Background(), "prompt")
	if err == nil {
		t.Fatal("Review: want error")
	}
	errorText := err.Error()
	if strings.Contains(errorText, apiKey) {
		t.Fatalf("error leaks api key: %q", errorText)
	}
	if strings.Contains(errorText, cfSecret) {
		t.Fatalf("error leaks cf secret: %q", errorText)
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
	errorBody         string
}

func newTestClient(t *testing.T) (*clyde.Client, *httptest.Server, *testServerState) {
	return newTestClientWithSleep(t, func(context.Context, time.Duration) error { return nil })
}

func newTestClientWithSleep(
	t *testing.T,
	sleep func(context.Context, time.Duration) error,
) (*clyde.Client, *httptest.Server, *testServerState) {
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
			body := state.errorBody
			if body == "" {
				body = "request failed"
			}
			http.Error(writer, body, status)
			return
		}

		writeJSON(writer, status, map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": state.completionContent,
					},
				},
			},
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

	client := clyde.NewClient(cfg, server.Client(), sleep)
	return client, server, state
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

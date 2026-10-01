package openai_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/openai"
)

// The client and the configuration loader run together against live HTTP
// peers. A disabled provider stays in the file and is never asked to answer.
func TestIntegrationDisabledProviderReceivesNoUsageFallback(t *testing.T) {
	var primaryCalls atomic.Int32
	var disabledCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		writeUsageExhausted(writer)
	}))
	t.Cleanup(primary.Close)
	disabled := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		disabledCalls.Add(1)
		writeReviewStream(writer, `{"overview":"This answer must not be used.","omissions_acceptable":true,"decision_reason":"Unused.","findings":[]}`)
	}))
	t.Cleanup(disabled.Close)

	configureRuntime(t, primary.URL, disabled.URL, true)
	cfg, err := config.FromEnvironment()
	if err != nil {
		t.Fatalf("FromEnvironment: %v", err)
	}
	client := openai.NewClient(cfg, http.DefaultClient)
	_, err = client.Review(context.Background(), "review this change")
	var providerError *openai.ProviderError
	if !errors.As(err, &providerError) || !providerError.UsageExceeded() {
		t.Fatalf("Review error = %v, want usage exhaustion from the enabled provider", err)
	}
	if primaryCalls.Load() != 1 || disabledCalls.Load() != 0 {
		t.Fatalf("provider calls = primary %d, disabled %d, want 1 and 0", primaryCalls.Load(), disabledCalls.Load())
	}
}

// An enabled second provider answers after the first reports exhausted usage.
func TestIntegrationEnabledProviderAnswersAfterUsageExhaustion(t *testing.T) {
	var primaryCalls atomic.Int32
	var secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		writeUsageExhausted(writer)
	}))
	t.Cleanup(primary.Close)
	secondary := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		writeReviewStream(writer, `{"overview":"The change is a single added line.","omissions_acceptable":true,"decision_reason":"Nothing blocks the change.","findings":[]}`)
	}))
	t.Cleanup(secondary.Close)

	configureRuntime(t, primary.URL, secondary.URL, false)
	cfg, err := config.FromEnvironment()
	if err != nil {
		t.Fatalf("FromEnvironment: %v", err)
	}
	client := openai.NewClient(cfg, http.DefaultClient)
	completion, err := client.Review(context.Background(), "review this change")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if completion.Model != "secondary-model" || primaryCalls.Load() != 1 || secondaryCalls.Load() != 1 {
		t.Fatalf("model = %q, calls = primary %d, secondary %d", completion.Model, primaryCalls.Load(), secondaryCalls.Load())
	}
}

func configureRuntime(t *testing.T, primaryURL string, secondaryURL string, disableSecondary bool) {
	t.Helper()
	privateKey := generatePrivateKey(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "runtime.json")
	configuration := map[string]any{
		"GITHUB_APP_ID":         "12345",
		"GITHUB_BOT_LOGIN":      "integration-review-agent[bot]",
		"PORT":                  "0",
		"REVIEW_MIN_IMPORTANCE": "8",
		"REVIEW_WORKERS":        "1",
		"PROVIDERS": []map[string]any{
			{"id": "primary", "base_url": strings.TrimRight(primaryURL, "/") + "/v1", "model": "primary-model", "api_key_binding": "PRIMARY_KEY"},
			{"id": "secondary", "base_url": strings.TrimRight(secondaryURL, "/") + "/v1", "model": "secondary-model", "api_key_binding": "SECONDARY_KEY", "disabled": disableSecondary},
		},
		"PROVIDER_PRIORITY":          []string{"primary", "secondary"},
		"SERVICE_FAILURE_APPEARANCE": map[string]string{"usage_exceeded": "pass"},
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		t.Fatalf("encode runtime: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	t.Setenv("RUNTIME_CONFIG_PATH", path)
	t.Setenv("GITHUB_PRIVATE_KEY", privateKey)
	t.Setenv("GITHUB_WEBHOOK_SECRET", "integration-webhook-secret")
	t.Setenv("PROVIDER_PRIMARY_API_KEY", "integration-primary-key")
	t.Setenv("PROVIDER_SECONDARY_API_KEY", "integration-secondary-key")
}

func generatePrivateKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func writeUsageExhausted(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{
			"message": "The usage limit has been reached",
			"type":    "invalid_request_error",
			"code":    "upstream_failed",
		},
	})
}

func writeReviewStream(writer http.ResponseWriter, content string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	frames := []map[string]any{
		{"type": "response.output_text.delta", "delta": content, "item_id": "item-test", "output_index": 0, "content_index": 0},
		{"type": "response.completed", "response": map[string]any{
			"id": "resp-test", "object": "response", "model": "secondary-model", "status": "completed",
			"output": []map[string]any{{
				"id": "item-test", "type": "message", "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "output_text", "text": content, "annotations": []any{}}},
			}},
		}},
	}
	for _, frame := range frames {
		encoded, _ := json.Marshal(frame)
		_, _ = writer.Write([]byte("event: " + frame["type"].(string) + "\ndata: " + string(encoded) + "\n\n"))
	}
}

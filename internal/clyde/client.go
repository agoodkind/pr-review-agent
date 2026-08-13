// Package clyde implements the direct Clyde chat completions client.
package clyde

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
)

const (
	maxResponseBytes = 16 * 1024 * 1024
	retryDelays      = 3
)

var retryBackoff = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
}

// APIError is a sanitized Clyde API failure with an HTTP status code.
type APIError struct {
	StatusCode int
	Message    string
}

// Error returns a sanitized Clyde API failure message.
func (err APIError) Error() string {
	return fmt.Sprintf("clyde api status %d: %s", err.StatusCode, err.Message)
}

// Client performs structured Clyde chat completion requests.
type Client struct {
	cfg        config.Config
	httpClient *http.Client
	sleep      func(context.Context, time.Duration) error
}

// NewClient constructs a Clyde client with injectable retry backoff.
func NewClient(
	cfg config.Config,
	httpClient *http.Client,
	sleep func(context.Context, time.Duration) error,
) *Client {
	return &Client{
		cfg:        cfg,
		httpClient: httpClient,
		sleep:      sleep,
	}
}

// Review requests one structured review completion.
func (client *Client) Review(ctx context.Context, prompt string) (domain.ReviewResult, error) {
	content, err := client.complete(ctx, prompt, reviewSchemaName, reviewJSONSchema())
	if err != nil {
		return domain.ReviewResult{}, err
	}

	result, err := decodeReviewResult(content)
	if err != nil {
		return domain.ReviewResult{}, err
	}
	if err := result.Validate(); err != nil {
		return domain.ReviewResult{}, errors.New("validate review result")
	}
	return result, nil
}

// Reconcile requests one structured thread reconciliation completion.
func (client *Client) Reconcile(ctx context.Context, prompt string) ([]domain.ThreadResolution, error) {
	content, err := client.complete(ctx, prompt, reconcileSchemaName, reconcileJSONSchema())
	if err != nil {
		return nil, err
	}

	response, err := decodeReconcileResponse(content)
	if err != nil {
		return nil, err
	}
	if err := validateReconcileResolutions(response.Resolutions); err != nil {
		return nil, err
	}
	return response.Resolutions, nil
}

type reconcileResponse struct {
	Resolutions []domain.ThreadResolution `json:"resolutions"`
}

type chatRequest struct {
	Model               string         `json:"model"`
	ReasoningEffort     string         `json:"reasoning_effort"`
	MaxCompletionTokens int            `json:"max_completion_tokens"`
	Messages            []chatMessage  `json:"messages"`
	ResponseFormat      responseFormat `json:"response_format"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

type chatChoice struct {
	Message chatMessage `json:"message"`
}

func (client *Client) complete(
	ctx context.Context,
	prompt string,
	schemaName string,
	schema json.RawMessage,
) (string, error) {
	requestBody, err := json.Marshal(chatRequest{
		Model:               config.Model,
		ReasoningEffort:     config.ReasoningEffort,
		MaxCompletionTokens: config.MaximumOutputTokens,
		Messages: []chatMessage{
			{Role: "system", Content: systemMessageContent()},
			{Role: "user", Content: prompt},
		},
		ResponseFormat: responseFormat{
			Type: "json_schema",
			JSONSchema: jsonSchema{
				Name:   schemaName,
				Strict: true,
				Schema: schema,
			},
		},
	})
	if err != nil {
		return "", errors.New("marshal clyde request")
	}

	target := strings.TrimRight(client.cfg.ClydeBaseURL.String(), "/") + "/chat/completions"

	var lastErr error
	for attempt := 0; attempt <= retryDelays; attempt++ {
		if attempt > 0 {
			if err := client.sleep(ctx, retryBackoff[attempt-1]); err != nil {
				return "", err
			}
		}

		content, statusCode, responseBody, requestErr := client.doOnce(ctx, target, requestBody)
		if requestErr != nil {
			lastErr = requestErr
			if attempt < retryDelays && isRetryableRequestError(requestErr) {
				continue
			}
			return "", requestErr
		}

		if statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices {
			return content, nil
		}

		apiErr := newAPIError(statusCode, sanitizeErrorBody(responseBody, client.cfg.ClydeAPIKey, client.cfg.CFAccessClientSecret))
		lastErr = apiErr
		if attempt < retryDelays && isRetryableStatus(statusCode) {
			continue
		}
		return "", apiErr
	}
	return "", lastErr
}

func (client *Client) doOnce(
	ctx context.Context,
	target string,
	requestBody []byte,
) (string, int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(requestBody))
	if err != nil {
		return "", 0, nil, errors.New("create clyde request")
	}
	request.Header.Set("Authorization", "Bearer "+client.cfg.ClydeAPIKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cf-Access-Client-Id", client.cfg.CFAccessClientID)
	request.Header.Set("Cf-Access-Client-Secret", client.cfg.CFAccessClientSecret)

	response, err := client.httpClient.Do(request)
	if err != nil {
		return "", 0, nil, errors.New("clyde request failed")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	responseBody, err := readLimitedBody(response.Body)
	if err != nil {
		return "", 0, nil, errors.New("read clyde response")
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", response.StatusCode, responseBody, nil
	}

	content, err := parseCompletionContent(responseBody)
	if err != nil {
		return "", 0, nil, err
	}
	return content, response.StatusCode, responseBody, nil
}

func parseCompletionContent(responseBody []byte) (string, error) {
	var response chatResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return "", errors.New("decode clyde response envelope")
	}
	if len(response.Choices) == 0 {
		return "", errors.New("clyde response missing choices")
	}
	content := strings.TrimSpace(response.Choices[0].Message.Content)
	if content == "" {
		return "", errors.New("clyde response missing message content")
	}
	return content, nil
}

func decodeReviewResult(content string) (domain.ReviewResult, error) {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var result domain.ReviewResult
	if err := decoder.Decode(&result); err != nil {
		return domain.ReviewResult{}, errors.New("decode structured output")
	}
	if decoder.More() {
		return domain.ReviewResult{}, errors.New("unexpected trailing json")
	}
	return result, nil
}

func decodeReconcileResponse(content string) (reconcileResponse, error) {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var response reconcileResponse
	if err := decoder.Decode(&response); err != nil {
		return reconcileResponse{}, errors.New("decode structured output")
	}
	if decoder.More() {
		return reconcileResponse{}, errors.New("unexpected trailing json")
	}
	return response, nil
}

func validateReconcileResolutions(resolutions []domain.ThreadResolution) error {
	seen := make(map[string]struct{}, len(resolutions))
	for _, resolution := range resolutions {
		if strings.TrimSpace(resolution.ThreadNodeID) == "" {
			return errors.New("thread_node_id is required")
		}
		if _, exists := seen[resolution.ThreadNodeID]; exists {
			return errors.New("duplicate thread_node_id")
		}
		seen[resolution.ThreadNodeID] = struct{}{}
		if _, err := domain.ParseResolution(string(resolution.Resolution)); err != nil {
			return errors.New("parse resolution")
		}
		if strings.TrimSpace(resolution.Reason) == "" {
			return errors.New("reason is required")
		}
	}
	return nil
}

func isRetryableRequestError(err error) bool {
	return err != nil
}

func isRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

func newAPIError(statusCode int, message string) APIError {
	return APIError{
		StatusCode: statusCode,
		Message:    message,
	}
}

func readLimitedBody(reader io.Reader) ([]byte, error) {
	limited := io.LimitReader(reader, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("read clyde response body")
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("clyde response too large")
	}
	return body, nil
}

func sanitizeErrorBody(body []byte, secrets ...string) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "request failed"
	}
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	for _, marker := range []string{"Bearer ", "sk-"} {
		if index := strings.Index(text, marker); index >= 0 {
			text = text[:index] + marker + "[redacted]"
		}
	}
	return text
}

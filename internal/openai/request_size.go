package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"

	"google.golang.org/genai"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/modelrequest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// ReviewRequestPayload exposes serialized model content without authentication headers.
type ReviewRequestPayload struct {
	ProviderID string
	API        config.ProviderAPI
	Body       json.RawMessage
}

// ReviewRequestBytes measures the largest request among enabled providers.
func (client *Client) ReviewRequestBytes(prompt string) (int, error) {
	payloads, err := client.ReviewRequestPayloads(prompt)
	return maximumPayloadBytes(payloads, err)
}

// ClassificationRequestBytes measures complete scope requests for enabled providers.
func (client *Client) ClassificationRequestBytes(prompt string) (int, error) {
	payloads, err := client.ClassificationRequestPayloads(prompt)
	return maximumPayloadBytes(payloads, err)
}

// ClassificationRequestPayloads uses the classification task and native request builders.
func (client *Client) ClassificationRequestPayloads(prompt string) ([]ReviewRequestPayload, error) {
	schema, err := MarshalClassificationSchema(client.reviewPolicy)
	if err != nil {
		return nil, err
	}
	return client.stageRequestPayloads(prompt, review.ClassificationPolicy, classificationSchemaName, schema)
}

func maximumPayloadBytes(payloads []ReviewRequestPayload, err error) (int, error) {
	if err != nil {
		return 0, err
	}
	maximum := 0
	for _, payload := range payloads {
		maximum = max(maximum, len(payload.Body))
	}
	return maximum, nil
}

// ReconcileRequestBytes measures complete reconciliation requests for enabled providers.
func (client *Client) ReconcileRequestBytes(prompt string) (int, error) {
	payloads, err := client.ReconcileRequestPayloads(prompt)
	return maximumPayloadBytes(payloads, err)
}

// ReconcileRequestPayloads serializes the configured reconciliation policy and schema.
func (client *Client) ReconcileRequestPayloads(prompt string) ([]ReviewRequestPayload, error) {
	return client.stageRequestPayloads(prompt, review.ReconciliationPolicy, reconcileSchemaName, reconcileSchemaJSON)
}

// ConsolidateRequestBytes measures complete finding consolidation requests.
func (client *Client) ConsolidateRequestBytes(prompt string) (int, error) {
	payloads, err := client.stageRequestPayloads(prompt, review.ConsolidationPolicy, consolidateSchemaName, consolidateSchemaJSON)
	return maximumPayloadBytes(payloads, err)
}

// ReportRequestBytes measures complete final report requests.
func (client *Client) ReportRequestBytes(prompt string) (int, error) {
	payloads, err := client.stageRequestPayloads(prompt, review.ReportPolicy, reportSchemaName, MarshalReportSchema(client.reviewPolicy))
	return maximumPayloadBytes(payloads, err)
}

func (client *Client) stageRequestPayloads(prompt string, renderPolicy func(reviewrules.Policy) (string, error), schemaName string, schema json.RawMessage) ([]ReviewRequestPayload, error) {
	policy, err := renderPolicy(client.reviewPolicy)
	if err != nil {
		slog.Warn("Request policy rendering failed", "schema", schemaName, "err", err)
		return nil, fmt.Errorf("render %s request policy: %w", schemaName, err)
	}
	instructions, err := structuredOutputPrompt(context.Background(), client.reviewPolicy, policy, schemaName, schema)
	if err != nil {
		return nil, err
	}
	return client.requestPayloads(prompt, instructions, schemaName, schema)
}

// ReviewRequestPayloads uses the configured instructions and native request builders.
func (client *Client) ReviewRequestPayloads(prompt string) ([]ReviewRequestPayload, error) {
	schema, err := MarshalReviewSchema(client.reviewPolicy)
	if err != nil {
		return nil, err
	}
	policy, err := review.PolicyHeader(client.reviewPolicy, client.minimumImportance, client.ruleImportance)
	if err != nil {
		slog.Warn("Review request policy rendering failed", "err", err)
		return nil, fmt.Errorf("render review request policy: %w", err)
	}
	instructions, err := structuredOutputPrompt(context.Background(), client.reviewPolicy, policy, reviewSchemaName, schema)
	if err != nil {
		return nil, err
	}
	return client.requestPayloads(prompt, instructions, reviewSchemaName, schema)
}

func (client *Client) requestPayloads(prompt, instructions, schemaName string, schema json.RawMessage) ([]ReviewRequestPayload, error) {
	if len(client.providers) == 0 {
		return nil, errors.New("request sizing requires an enabled provider")
	}
	payloads := make([]ReviewRequestPayload, 0, len(client.providers))
	for _, target := range client.providers {
		body, err := marshalProviderRequest(target, prompt, instructions, schemaName, schema)
		if err != nil {
			slog.Warn("Provider request serialization failed", "provider_id", target.id, "err", err)
			return nil, fmt.Errorf("serialize provider %s request: %w", target.id, err)
		}
		payloads = append(payloads, ReviewRequestPayload{ProviderID: target.id, API: target.api, Body: body})
	}
	return payloads, nil
}

func (client *Client) checkRequestSizes(ctx context.Context, prompt, instructions, schemaName string, schema json.RawMessage) error {
	payloads, err := client.requestPayloads(prompt, instructions, schemaName, schema)
	if err != nil {
		return err
	}
	maximum := modelrequest.MaximumBytes(ctx, client.maximumRequestBytes)
	for _, payload := range payloads {
		if len(payload.Body) > maximum {
			return &diff.PromptSizeError{Actual: len(payload.Body), Limit: maximum}
		}
	}
	return nil
}

func marshalProviderRequest(target provider, prompt, instructions, schemaName string, schema json.RawMessage) (json.RawMessage, error) {
	if target.api == config.GeminiAPI {
		return marshalGeminiRequest(target, prompt, instructions, schema)
	}
	var encoded []byte
	var err error
	if target.api == config.ChatCompletionsAPI {
		encoded, err = json.Marshal(newChatParams(target, prompt, instructions, schemaName, schema))
	} else {
		params, paramsErr := newResponseParams(target, prompt, instructions, schemaName, schema)
		if paramsErr != nil {
			return nil, paramsErr
		}
		encoded, err = json.Marshal(params)
	}
	if err != nil {
		return nil, requestEncodingError(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &payload); err != nil {
		return nil, requestEncodingError(err)
	}
	payload["stream"] = json.RawMessage("true")
	fields, enabled := responseRouting(target)
	if target.api == config.ResponsesAPI && enabled {
		encoded, err := json.Marshal(fields)
		if err != nil {
			return nil, requestEncodingError(err)
		}
		var extras map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &extras); err != nil {
			return nil, requestEncodingError(err)
		}
		maps.Copy(payload, extras)
	}
	body, err := json.Marshal(payload)
	return body, requestEncodingError(err)
}

type responseRoutingPlugin struct {
	ID       string `json:"id"`
	CostTier string `json:"cost_tier"`
}

type responseRoutingProvider struct {
	RequireParameters bool `json:"require_parameters"`
}

type responseRoutingConfiguration struct {
	Plugins  []responseRoutingPlugin `json:"plugins"`
	Provider responseRoutingProvider `json:"provider"`
}

func responseRouting(target provider) (responseRoutingConfiguration, bool) {
	if target.autoRouterCostTier == "" {
		var empty responseRoutingConfiguration
		return empty, false
	}
	return responseRoutingConfiguration{
		Plugins:  []responseRoutingPlugin{{ID: "auto-router", CostTier: string(target.autoRouterCostTier)}},
		Provider: responseRoutingProvider{RequireParameters: true},
	}, true
}

func marshalGeminiRequest(target provider, prompt, instructions string, schema json.RawMessage) (json.RawMessage, error) {
	params, err := newGeminiParams(target, instructions, schema)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, requestEncodingError(err)
	}
	var generation map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &generation); err != nil {
		return nil, requestEncodingError(err)
	}
	system := generation["systemInstruction"]
	delete(generation, "systemInstruction")
	delete(generation, "httpOptions")
	configuration, err := json.Marshal(generation)
	if err != nil {
		return nil, requestEncodingError(err)
	}
	contents, err := json.Marshal([]*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)})
	if err != nil {
		return nil, requestEncodingError(err)
	}
	body, err := json.Marshal(map[string]json.RawMessage{"contents": contents, "systemInstruction": system, "generationConfig": configuration})
	return body, requestEncodingError(err)
}

func requestEncodingError(err error) error {
	if err == nil {
		return nil
	}
	slog.Warn("Provider request encoding failed", "err", err)
	return fmt.Errorf("encode provider request: %w", err)
}

func requestTransportError(err error) error {
	if err == nil {
		return nil
	}
	slog.Warn("Provider request transport failed", "err", err)
	return fmt.Errorf("send provider request: %w", err)
}

type requestSizeTransport struct {
	transport http.RoundTripper
	maximum   int
}

func requestBoundHTTPClient(client *http.Client, maximum int) *http.Client {
	bounded := *client
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	bounded.Transport = requestSizeTransport{transport: transport, maximum: maximum}
	return &bounded
}

// RoundTrip checks the actual SDK body before the transport sends model content.
func (transport requestSizeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	maximum := modelrequest.MaximumBytes(request.Context(), transport.maximum)
	if request.Body == nil {
		response, err := transport.transport.RoundTrip(request)
		return response, requestTransportError(err)
	}
	if request.ContentLength > int64(maximum) {
		_ = request.Body.Close()
		return nil, &diff.PromptSizeError{Actual: int(request.ContentLength), Limit: maximum}
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, int64(maximum)+1))
	if err != nil {
		_ = request.Body.Close()
		return nil, requestTransportError(err)
	}
	if err := request.Body.Close(); err != nil {
		return nil, requestTransportError(err)
	}
	if len(body) > maximum {
		return nil, &diff.PromptSizeError{Actual: len(body), Limit: maximum}
	}
	bounded := request.Clone(request.Context())
	bounded.Body = io.NopCloser(bytes.NewReader(body))
	response, err := transport.transport.RoundTrip(bounded)
	return response, requestTransportError(err)
}

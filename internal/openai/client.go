// Package openai calls OpenAI chat completions for review and reconciliation.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	openaigo "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
)

const maxRetries = 3

// Client performs structured OpenAI chat completion requests.
type Client struct {
	sdk openaigo.Client
}

// NewClient constructs an OpenAI SDK client from service config.
func NewClient(cfg config.Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.ClydeAPIKey),
		option.WithHTTPClient(httpClient),
		option.WithHeader("Cf-Access-Client-Id", cfg.CFAccessClientID),
		option.WithHeader("Cf-Access-Client-Secret", cfg.CFAccessClientSecret),
		option.WithMaxRetries(maxRetries),
	}
	if cfg.ClydeBaseURL != nil {
		opts = append(opts, option.WithBaseURL(strings.TrimRight(cfg.ClydeBaseURL.String(), "/")+"/"))
	}
	return &Client{
		sdk: openaigo.NewClient(opts...),
	}
}

// Review requests one structured review completion.
func (client *Client) Review(ctx context.Context, prompt string) (domain.ReviewResult, error) {
	content, err := client.complete(ctx, prompt, reviewSchemaName, reviewSchemaJSON)
	if err != nil {
		return domain.ReviewResult{}, err
	}
	var result domain.ReviewResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return domain.ReviewResult{}, errors.New("decode structured output")
	}
	if err := result.Validate(); err != nil {
		return domain.ReviewResult{}, errors.New("validate review result")
	}
	return result, nil
}

// Reconcile requests one structured thread reconciliation completion.
func (client *Client) Reconcile(ctx context.Context, prompt string) ([]domain.ThreadResolution, error) {
	content, err := client.complete(ctx, prompt, reconcileSchemaName, reconcileSchemaJSON)
	if err != nil {
		return nil, err
	}
	var response struct {
		Resolutions []domain.ThreadResolution `json:"resolutions"`
	}
	if err := json.Unmarshal([]byte(content), &response); err != nil {
		return nil, errors.New("decode structured output")
	}
	if err := validateReconcileResolutions(response.Resolutions); err != nil {
		return nil, err
	}
	return response.Resolutions, nil
}

func (client *Client) complete(
	ctx context.Context,
	prompt string,
	schemaName string,
	schema json.RawMessage,
) (string, error) {
	completion, err := client.sdk.Chat.Completions.New(ctx, openaigo.ChatCompletionNewParams{
		Model:               shared.ChatModel(config.Model),
		ReasoningEffort:     shared.ReasoningEffort(config.ReasoningEffort),
		MaxCompletionTokens: openaigo.Int(int64(config.MaximumOutputTokens)),
		Messages: []openaigo.ChatCompletionMessageParamUnion{
			openaigo.SystemMessage(systemMessageContent()),
			openaigo.UserMessage(prompt),
		},
		ResponseFormat: openaigo.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &openaigo.ResponseFormatJSONSchemaParam{
				JSONSchema: openaigo.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   schemaName,
					Strict: openaigo.Bool(true),
					Schema: schema,
				},
			},
		},
	})
	if err != nil {
		return "", errors.New("openai chat completion failed")
	}
	if len(completion.Choices) == 0 {
		return "", errors.New("openai response missing choices")
	}
	content := strings.TrimSpace(completion.Choices[0].Message.Content)
	if content == "" {
		return "", errors.New("openai response missing message content")
	}
	return content, nil
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

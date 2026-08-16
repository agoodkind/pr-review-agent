// Package openai calls OpenAI chat completions for review and reconciliation.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	openaigo "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/review"
)

const maxRetries = 3

// Client performs structured OpenAI chat completion requests.
type Client struct {
	sdk               openaigo.Client
	minimumImportance int
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
		sdk:               openaigo.NewClient(opts...),
		minimumImportance: cfg.MinimumImportance,
	}
}

// Review requests one structured review completion.
func (client *Client) Review(ctx context.Context, prompt string) (domain.ReviewResult, error) {
	content, err := client.complete(
		ctx,
		prompt,
		review.PolicyHeader(client.minimumImportance),
		reviewSchemaName,
		reviewSchemaJSON,
	)
	if err != nil {
		return domain.ReviewResult{}, err
	}
	var result domain.ReviewResult
	decoder := json.NewDecoder(strings.NewReader(content))
	if err := decoder.Decode(&result); err != nil {
		return domain.ReviewResult{}, errors.New("decode structured output: " + err.Error())
	}
	if err := result.Validate(); err != nil {
		return domain.ReviewResult{}, errors.New("validate review result: " + err.Error())
	}
	return result, nil
}

// Reconcile requests one structured thread reconciliation completion.
func (client *Client) Reconcile(ctx context.Context, prompt string) ([]domain.ThreadResolution, error) {
	content, err := client.complete(
		ctx,
		prompt,
		review.ReconciliationPolicy(),
		reconcileSchemaName,
		reconcileSchemaJSON,
	)
	if err != nil {
		return nil, err
	}
	var response struct {
		Resolutions []domain.ThreadResolution `json:"resolutions"`
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	if err := decoder.Decode(&response); err != nil {
		return nil, errors.New("decode structured output: " + err.Error())
	}
	if err := domain.ValidateThreadResolutions(response.Resolutions); err != nil {
		return nil, errors.New("validate thread resolutions: " + err.Error())
	}
	return response.Resolutions, nil
}

func (client *Client) complete(
	ctx context.Context,
	prompt string,
	policy string,
	schemaName string,
	schema json.RawMessage,
) (string, error) {
	stream := client.sdk.Chat.Completions.NewStreaming(ctx, openaigo.ChatCompletionNewParams{
		Model:               shared.ChatModel(config.Model),
		ReasoningEffort:     shared.ReasoningEffort(config.ReasoningEffort),
		MaxCompletionTokens: openaigo.Int(int64(config.MaximumOutputTokens)),
		Messages: []openaigo.ChatCompletionMessageParamUnion{
			openaigo.SystemMessage(structuredOutputPrompt(policy, schemaName, schema)),
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
	defer func() {
		_ = stream.Close()
	}()

	var content strings.Builder
	finishReason := ""
	for stream.Next() {
		chunk := stream.Current()
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			content.WriteString(choice.Delta.Content)
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
		}
	}
	if err := stream.Err(); err != nil {
		return "", modelProviderError(err)
	}
	if finishReason == "" {
		return "", errors.New("openai response ended without a finish reason")
	}
	if finishReason != "stop" {
		return "", errors.New("openai response stopped with finish reason " + finishReason)
	}
	result := strings.TrimSpace(content.String())
	if result == "" {
		return "", errors.New("openai response missing message content")
	}
	return result, nil
}

func modelProviderError(err error) error {
	var apiError *openaigo.Error
	if errors.As(err, &apiError) {
		details := []string{fmt.Sprintf(
			"model provider returned HTTP %d %s",
			apiError.StatusCode,
			http.StatusText(apiError.StatusCode),
		)}
		for _, value := range []string{apiError.Type, apiError.Code, apiError.Param} {
			value = strings.Join(strings.Fields(value), " ")
			if value != "" {
				details = append(details, value)
			}
		}
		return errors.New(strings.Join(details, ": "))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("model provider request timed out")
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("model provider request was cancelled")
	}
	return errors.New("model provider request failed before receiving a response")
}

func structuredOutputPrompt(policy string, schemaName string, schema json.RawMessage) string {
	return policy +
		"\n\nReturn only JSON. Do not use Markdown fences or add prose. " +
		"The JSON must validate against schema " + schemaName + ":\n" + string(schema)
}

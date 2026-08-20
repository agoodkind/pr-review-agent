// Package openai calls OpenAI chat completions for review and reconciliation.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	openaigo "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/review"
)

const maxRetries = 3

// provider is one model endpoint the client can send a completion to.
type provider struct {
	sdk   openaigo.Client
	model shared.ChatModel
}

// Client performs structured OpenAI chat completion requests.
type Client struct {
	primary                 provider
	fallback                *provider
	fallbackOnUsageExceeded bool
	minimumImportance       int
}

// NewClient constructs an OpenAI SDK client from service config. It builds a
// second provider when the configuration names a fallback endpoint.
func NewClient(cfg config.Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := &Client{
		primary: provider{
			sdk: newProviderSDK(
				httpClient,
				cfg.ClydeBaseURL,
				cfg.ClydeAPIKey,
				cfg.CFAccessClientID,
				cfg.CFAccessClientSecret,
			),
			model: cfg.ReviewModel,
		},
		fallback:                nil,
		fallbackOnUsageExceeded: false,
		minimumImportance:       cfg.MinimumImportance,
	}
	if cfg.HasFallback() {
		client.fallback = &provider{
			sdk: newProviderSDK(
				httpClient,
				cfg.FallbackBaseURL,
				cfg.FallbackAPIKey,
				cfg.FallbackCFAccessClientID,
				cfg.FallbackCFAccessClientSecret,
			),
			model: cfg.FallbackModel,
		}
		client.fallbackOnUsageExceeded = cfg.FallbackOnUsageExceeded
	}
	return client
}

// newProviderSDK builds one SDK handle. It sends the Cloudflare Access headers
// only when both values are present, because a public endpoint needs none.
func newProviderSDK(
	httpClient *http.Client,
	baseURL *url.URL,
	apiKey string,
	cfAccessClientID string,
	cfAccessClientSecret string,
) openaigo.Client {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(maxRetries),
	}
	if cfAccessClientID != "" && cfAccessClientSecret != "" {
		opts = append(
			opts,
			option.WithHeader("Cf-Access-Client-Id", cfAccessClientID),
			option.WithHeader("Cf-Access-Client-Secret", cfAccessClientSecret),
		)
	}
	if baseURL != nil {
		opts = append(opts, option.WithBaseURL(strings.TrimRight(baseURL.String(), "/")+"/"))
	}
	return openaigo.NewClient(opts...)
}

// Review requests one structured review completion and reports the model that
// served it, which is the fallback model whenever the primary refused.
func (client *Client) Review(ctx context.Context, prompt string) (review.Completion, error) {
	content, model, err := client.complete(
		ctx,
		prompt,
		review.PolicyHeader(client.minimumImportance),
		reviewSchemaName,
		reviewSchemaJSON,
	)
	if err != nil {
		return review.Completion{}, err
	}
	var result domain.ReviewResult
	decoder := json.NewDecoder(strings.NewReader(content))
	if err := decoder.Decode(&result); err != nil {
		return review.Completion{}, errors.New("decode structured output: " + err.Error())
	}
	if err := result.Validate(); err != nil {
		return review.Completion{}, errors.New("validate review result: " + err.Error())
	}
	return review.Completion{Result: result, Model: model}, nil
}

// Reconcile requests one structured thread reconciliation completion.
func (client *Client) Reconcile(ctx context.Context, prompt string) ([]domain.ThreadResolution, error) {
	content, _, err := client.complete(
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

// complete sends one request to the primary provider and repeats it against the
// fallback when the primary refusal matches the declared fallback condition.
// It returns the content and the model that produced it. It keeps no memory of
// a refusal, so the primary is used again as soon as it recovers.
func (client *Client) complete(
	ctx context.Context,
	prompt string,
	policy string,
	schemaName string,
	schema json.RawMessage,
) (string, string, error) {
	content, primaryErr := client.completeWith(ctx, client.primary, prompt, policy, schemaName, schema)
	if primaryErr == nil {
		return content, client.primary.model, nil
	}
	if !client.shouldUseFallback(primaryErr) {
		return "", "", primaryErr
	}

	logger := gklog.L(ctx)
	logger.WarnContext(
		ctx,
		"model provider fallback engaged",
		slog.String("err", primaryErr.Error()),
	)
	content, fallbackErr := client.completeWith(ctx, *client.fallback, prompt, policy, schemaName, schema)
	if fallbackErr != nil {
		return "", "", errors.Join(primaryErr, fallbackErr)
	}
	return content, client.fallback.model, nil
}

// shouldUseFallback reports whether this failure is the declared condition for
// sending the request to the fallback provider.
func (client *Client) shouldUseFallback(err error) bool {
	if client.fallback == nil {
		return false
	}
	if !client.fallbackOnUsageExceeded {
		return false
	}
	var providerError *ProviderError
	if !errors.As(err, &providerError) {
		return false
	}
	return providerError.UsageExceeded()
}

func (client *Client) completeWith(
	ctx context.Context,
	target provider,
	prompt string,
	policy string,
	schemaName string,
	schema json.RawMessage,
) (string, error) {
	stream := target.sdk.Chat.Completions.NewStreaming(ctx, openaigo.ChatCompletionNewParams{
		Model:               target.model,
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
		return &ProviderError{
			StatusCode: apiError.StatusCode,
			Type:       apiError.Type,
			Code:       apiError.Code,
			Param:      apiError.Param,
			Message:    apiError.Message,
		}
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

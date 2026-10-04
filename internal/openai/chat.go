package openai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	openaigo "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/review"
)

func completeChat(
	ctx context.Context,
	target provider,
	prompt string,
	policy string,
	schemaName string,
	schema json.RawMessage,
	report func(int64),
) (string, string, error) {
	params := openaigo.ChatCompletionNewParams{
		Model: target.model,
		Messages: []openaigo.ChatCompletionMessageParamUnion{
			openaigo.SystemMessage(structuredOutputPrompt(policy, schemaName, schema)),
			openaigo.UserMessage(prompt),
		},
		ResponseFormat: openaigo.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name: schemaName, Strict: openaigo.Bool(true), Schema: schema,
				},
			},
		},
		StreamOptions: openaigo.ChatCompletionStreamOptionsParam{IncludeUsage: openaigo.Bool(true)},
	}
	if !target.omitMaxOutputTokens {
		maxOutputTokens := target.maxOutputTokens
		if maxOutputTokens == 0 {
			maxOutputTokens = config.MaximumOutputTokens
		}
		params.MaxTokens = openaigo.Int(maxOutputTokens)
	}
	stream := target.sdk.Chat.Completions.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()

	var content strings.Builder
	model := target.model
	usage := openaigo.CompletionUsage{}
	hasUsage := false
	finished := false
	finishReason := ""
	defer func() {
		converted := responseUsageFromChat(usage)
		result := modelUsage(model, converted, hasUsage, target)
		result.AudioInputTokens = usage.PromptTokensDetails.AudioTokens
		result.AudioOutputTokens = usage.CompletionTokensDetails.AudioTokens
		result.AcceptedPredictionTokens = usage.CompletionTokensDetails.AcceptedPredictionTokens
		result.RejectedPredictionTokens = usage.CompletionTokensDetails.RejectedPredictionTokens
		review.RecordModelUsage(ctx, result)
		if hasUsage {
			report(budgetTokens(converted, target.dailyTokenTypes))
		}
	}()
	for stream.Next() {
		chunk := stream.Current()
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.JSON.Usage.Valid() {
			usage = chunk.Usage
			hasUsage = true
		}
		for _, choice := range chunk.Choices {
			content.WriteString(choice.Delta.Content)
			if choice.FinishReason != "" {
				finished = true
				finishReason = choice.FinishReason
			}
		}
	}
	if err := stream.Err(); err != nil {
		return "", "", chatProviderError(target.model, err)
	}
	if finishReason == "length" {
		return "", "", &TruncatedError{Model: target.model}
	}
	if !finished || finishReason != "stop" {
		return "", "", errors.New("chat completion ended without a successful finish reason")
	}
	result := strings.TrimSpace(content.String())
	if result == "" {
		return "", "", errors.New("chat completion missing message content")
	}
	return result, model, nil
}

func chatProviderError(model string, err error) error {
	converted := modelProviderError(model, err)
	var apiError *openaigo.Error
	if !errors.As(err, &apiError) {
		return converted
	}
	var payload struct {
		Status string `json:"status"`
		Error  struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(apiError.RawJSON()), &payload) != nil {
		return converted
	}
	if payload.Status == "RESOURCE_EXHAUSTED" || payload.Error.Status == "RESOURCE_EXHAUSTED" {
		var providerError *ProviderError
		if errors.As(converted, &providerError) {
			providerError.Code = "RESOURCE_EXHAUSTED"
		}
	}
	return converted
}

func responseUsageFromChat(usage openaigo.CompletionUsage) responses.ResponseUsage {
	return responses.ResponseUsage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
		InputTokensDetails: responses.ResponseUsageInputTokensDetails{
			CachedTokens: usage.PromptTokensDetails.CachedTokens,
		},
		OutputTokensDetails: responses.ResponseUsageOutputTokensDetails{
			ReasoningTokens: usage.CompletionTokensDetails.ReasoningTokens,
		},
	}
}

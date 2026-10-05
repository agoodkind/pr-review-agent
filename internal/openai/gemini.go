package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/responses"
	"google.golang.org/genai"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/review"
)

func completeGemini(
	ctx context.Context,
	target provider,
	prompt string,
	policy string,
	schema json.RawMessage,
	report func(responses.ResponseUsage),
) (string, string, error) {
	attempts := int32(1)
	sdk, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:     target.apiKey,
		Backend:    genai.BackendGeminiAPI,
		HTTPClient: target.httpClient,
		HTTPOptions: genai.HTTPOptions{
			BaseURL: target.baseURL.String(),
			RetryOptions: &genai.HTTPRetryOptions{
				Attempts: &attempts,
			},
		},
	})
	if err != nil {
		logger := gklog.L(ctx)
		logger.WarnContext(ctx, "Gemini client creation failed")
		return "", "", fmt.Errorf("create Gemini client: %w", err)
	}
	params, err := newGeminiParams(target, policy, schema)
	if err != nil {
		return "", "", err
	}

	var content strings.Builder
	model := target.model
	usage := responses.ResponseUsage{}
	hasUsage := false
	finishReason := genai.FinishReasonUnspecified
	defer func() {
		review.RecordModelUsage(ctx, modelUsage(model, usage, hasUsage, target))
		if hasUsage {
			report(usage)
		}
	}()
	contents := []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)}
	for chunk, streamErr := range sdk.Models.GenerateContentStream(ctx, target.model, contents, params) {
		if streamErr != nil {
			return "", "", geminiProviderError(streamErr)
		}
		if chunk.ModelVersion != "" {
			model = chunk.ModelVersion
		}
		if chunk.UsageMetadata != nil {
			usage = responseUsageFromGemini(chunk.UsageMetadata)
			hasUsage = true
		}
		for _, candidate := range chunk.Candidates {
			if candidate.FinishReason != "" {
				finishReason = candidate.FinishReason
			}
			appendGeminiContent(&content, candidate.Content)
		}
	}
	if finishReason == genai.FinishReasonMaxTokens {
		return "", "", &TruncatedError{Model: target.model}
	}
	if finishReason != genai.FinishReasonStop {
		return "", "", fmt.Errorf("gemini completion ended without a successful finish reason: %s", finishReason)
	}
	result := strings.TrimSpace(content.String())
	if result == "" {
		return "", "", errors.New("gemini completion missing message content")
	}
	return result, model, nil
}

func appendGeminiContent(builder *strings.Builder, content *genai.Content) {
	if content == nil {
		return
	}
	for _, part := range content.Parts {
		if !part.Thought {
			builder.WriteString(part.Text)
		}
	}
}

func newGeminiParams(target provider, policy string, schema json.RawMessage) (*genai.GenerateContentConfig, error) {
	level, err := geminiThinkingLevel(target.reasoningEffort)
	if err != nil {
		return nil, err
	}
	params := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(policy, genai.RoleUser),
		ThinkingConfig:    &genai.ThinkingConfig{ThinkingLevel: level},
	}
	if !target.omitMaxOutputTokens {
		limit := target.maxOutputTokens
		if limit == 0 {
			limit = config.MaximumOutputTokens
		}
		params.MaxOutputTokens = int32(limit)
	}
	if !target.omitTextFormat {
		params.ResponseMIMEType = "application/json"
		params.ResponseJsonSchema = schema
	}
	return params, nil
}

func geminiThinkingLevel(effort config.ReasoningEffort) (genai.ThinkingLevel, error) {
	switch effort {
	case config.ReasoningMinimal:
		return genai.ThinkingLevelMinimal, nil
	case config.ReasoningLow:
		return genai.ThinkingLevelLow, nil
	case config.ReasoningMedium:
		return genai.ThinkingLevelMedium, nil
	case config.ReasoningHigh:
		return genai.ThinkingLevelHigh, nil
	case config.ReasoningNone, config.ReasoningXHigh:
	}
	return genai.ThinkingLevelUnspecified, fmt.Errorf("unsupported Gemini reasoning effort: %s", effort)
}

func geminiProviderError(err error) error {
	var apiError genai.APIError
	if errors.As(err, &apiError) {
		return &ProviderError{
			StatusCode: apiError.Code,
			Type:       "",
			Code:       apiError.Status,
			Param:      "",
			Message:    apiError.Message,
		}
	}
	return err
}

func responseUsageFromGemini(usage *genai.GenerateContentResponseUsageMetadata) responses.ResponseUsage {
	return responses.ResponseUsage{
		InputTokens:  int64(usage.PromptTokenCount),
		OutputTokens: int64(usage.CandidatesTokenCount) + int64(usage.ThoughtsTokenCount),
		TotalTokens:  int64(usage.TotalTokenCount),
		InputTokensDetails: responses.ResponseUsageInputTokensDetails{
			CachedTokens: int64(usage.CachedContentTokenCount),
		},
		OutputTokensDetails: responses.ResponseUsageOutputTokensDetails{
			ReasoningTokens: int64(usage.ThoughtsTokenCount),
		},
	}
}

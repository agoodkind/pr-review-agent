package openai

import (
	"errors"
	"strings"

	"google.golang.org/genai"

	"goodkind.io/pr-review-agent/internal/config"
)

var (
	errResponseNotCompleted   = errors.New("openai response ended without a completion event")
	errResponseContentMissing = errors.New("openai response missing message content")
	errGeminiContentMissing   = errors.New("gemini completion missing message content")
)

type incompleteResponseError struct{ reason string }

func (err *incompleteResponseError) Error() string {
	return "openai response incomplete: " + err.reason
}

type geminiFinishError struct{ reason genai.FinishReason }

func (err *geminiFinishError) Error() string {
	return "gemini completion ended without a successful finish reason: " + string(err.reason)
}

func responseCompletion(
	target provider,
	content *strings.Builder,
	responseModel string,
	completed bool,
	incompleteReason string,
) (string, string, error) {
	if incompleteReason == "max_output_tokens" {
		return "", "", &TruncatedError{Model: target.model}
	}

	if incompleteReason != "" {
		return "", "", &incompleteResponseError{reason: incompleteReason}
	}

	if !completed {
		return "", "", errResponseNotCompleted
	}

	result := strings.TrimSpace(content.String())
	if result == "" {
		return "", "", errResponseContentMissing
	}

	completionModel := target.model
	if (target.model == config.AutoRouterModel || target.model == config.FreeRouterModel) &&
		responseModel != "" {
		completionModel = responseModel
	}

	return result, completionModel, nil
}

func geminiCompletion(
	target provider,
	content *strings.Builder,
	model string,
	finishReason genai.FinishReason,
) (string, string, error) {
	if finishReason == genai.FinishReasonMaxTokens {
		return "", "", &TruncatedError{Model: target.model}
	}

	if finishReason != genai.FinishReasonStop {
		return "", "", &geminiFinishError{reason: finishReason}
	}

	result := strings.TrimSpace(content.String())
	if result == "" {
		return "", "", errGeminiContentMissing
	}

	if model == "" {
		return result, target.model, nil
	}

	return result, model, nil
}

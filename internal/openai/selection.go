package openai

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/openai/openai-go/v3/responses"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/modelrequest"
)

// ManagesProviderTimeouts prevents a shared stage deadline from cancelling fallback attempts.
func (*Client) ManagesProviderTimeouts() bool { return true }

func resolveRequestTimeout(timeout time.Duration) time.Duration {
	if timeout == 0 {
		return config.DefaultReviewChunkTimeout
	}
	return timeout
}

type providerPanicError struct{}

func (*providerPanicError) Error() string {
	return "model provider adapter panicked"
}

func (client *Client) attemptCompletion(
	ctx context.Context,
	target provider,
	prompt string,
	instructions string,
	schemaName string,
	schema json.RawMessage,
	accept func(string) error,
) (content string, model string, budget budgetSnapshot, err error) {
	timeout := target.requestTimeout
	if timeout == 0 {
		timeout = modelrequest.Timeout(ctx, client.requestTimeout)
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			content, model, err = "", "", &providerPanicError{}
		}
		if timeoutErr := attemptCtx.Err(); timeoutErr != nil {
			err = errors.Join(err, timeoutErr)
		}
	}()
	budget, err = client.checkBudget(attemptCtx, target)
	if err != nil {
		return "", "", budget, err
	}
	report := func(usage responses.ResponseUsage) {
		client.reportBudget(attemptCtx, target, budget, usage)
	}
	switch target.api {
	case config.GeminiAPI:
		content, model, err = completeGemini(attemptCtx, target, prompt, instructions, schema, report)
	case config.ChatCompletionsAPI:
		content, model, err = completeChat(attemptCtx, target, prompt, instructions, schemaName, schema, report)
	case config.ResponsesAPI:
		content, model, err = completeWith(attemptCtx, target, prompt, instructions, schemaName, schema, report)
	}
	if err == nil {
		err = accept(content)
	}
	return content, model, budget, err
}

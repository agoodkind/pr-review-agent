package openai

import (
	"context"
	"fmt"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/review"
)

var _ review.FileContentClassifier = (*Client)(nil)

// ClassifyFileContent requests a scope decision with the configured provider chain.
func (client *Client) ClassifyFileContent(ctx context.Context, prompt string) (review.Completion, error) {
	logger := gklog.L(ctx)
	schema, err := MarshalClassificationSchema(client.reviewPolicy)
	if err != nil {
		return review.Completion{}, err
	}
	policy, err := review.ClassificationPolicy(client.reviewPolicy)
	if err != nil {
		logger.WarnContext(ctx, "Classification policy rendering failed")
		return review.Completion{}, fmt.Errorf("render classification policy: %w", err)
	}
	var result domain.ReviewResult
	model, err := client.complete(ctx, prompt, policy, classificationSchemaName, schema, func(content string) error {
		candidate, err := domain.UnmarshalClassificationResult([]byte(content))
		if err != nil {
			logger.WarnContext(ctx, "Model classification result validation failed")
			return fmt.Errorf("decode model classification result: %w", err)
		}
		result = candidate
		return nil
	})
	if err != nil {
		return review.Completion{}, err
	}
	return review.Completion{Result: result, Model: model}, nil
}

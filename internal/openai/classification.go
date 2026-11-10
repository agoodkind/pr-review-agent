package openai

import (
	"context"
	"fmt"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/review"
)

var _ review.FileContentClassifier = (*Client)(nil)

// ClassifyFileContent requests a file scope decision.
//
// The client uses the configured provider chain and a schema that forbids
// review findings.
//
// For example:
//
//	completion, err := client.ClassifyFileContent(ctx, prompt)
func (client *Client) ClassifyFileContent(
	ctx context.Context,
	prompt string,
) (review.Completion, error) {
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

	model, err := client.complete(
		ctx,
		prompt,
		policy,
		classificationSchemaName,
		schema,
		func(content string) error {
			candidate, err := domain.UnmarshalClassificationResult([]byte(content))
			if err != nil {
				logger.WarnContext(ctx, "Model classification result validation failed")
				return fmt.Errorf("decode model classification result: %w", err)
			}

			requests, err := inspectionRequests(ctx, []byte(content), classificationSchemaName)
			if err != nil {
				return err
			}

			if len(requests) > 0 {
				return &domain.FileAssessmentValidationError{Field: "inspection_requests"}
			}

			result = candidate

			return nil
		},
	)
	if err != nil {
		return review.Completion{}, err
	}

	return review.Completion{Result: result, Model: model, InspectionRequests: nil}, nil
}

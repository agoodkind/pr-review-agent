package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/review"
)

const metadataAssessmentSchemaName = "metadata_finding_assessments"

var metadataAssessmentSchema = json.RawMessage(
	`{"type":"object","additionalProperties":false,"properties":{"assessments":{"type":"array","items":{"type":"object","additionalProperties":false,"properties":{"finding_id":{"type":"string"},"resolution":{"type":"string","enum":["resolved","open","uncertain"]},"reason":{"type":"string"},"evidence":{"type":"string"},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}},"required":["finding_id","resolution","reason","evidence","start_line","end_line"]}}},"required":["assessments"]}`,
)

type metadataAssessmentJSON struct {
	FindingID  *string            `json:"finding_id"`
	Resolution *domain.Resolution `json:"resolution"`
	Reason     *string            `json:"reason"`
	Evidence   *string            `json:"evidence"`
	StartLine  *int               `json:"start_line"`
	EndLine    *int               `json:"end_line"`
}

// MetadataReassessmentRequestBytes measures all configured provider requests.
func (client *Client) MetadataReassessmentRequestBytes(prompt string) (int, error) {
	payloads, err := client.stageRequestPayloads(
		prompt,
		review.MetadataReassessmentPolicy,
		metadataAssessmentSchemaName,
		metadataAssessmentSchema,
	)

	return maximumPayloadBytes(payloads, err)
}

// ReassessMetadata requests explicit decisions about saved metadata findings.
func (client *Client) ReassessMetadata(
	ctx context.Context,
	request review.MetadataReassessmentRequest,
) ([]domain.MetadataFindingAssessment, error) {
	logger := gklog.L(ctx)
	policy, err := review.MetadataReassessmentPolicy(client.reviewPolicy)
	if err != nil {
		logger.WarnContext(ctx, "Metadata reassessment policy rendering failed", "err", err)
		return nil, fmt.Errorf("render metadata reassessment policy: %w", err)
	}

	var assessments []domain.MetadataFindingAssessment
	_, err = client.complete(
		ctx,
		request.Prompt,
		policy,
		metadataAssessmentSchemaName,
		metadataAssessmentSchema,
		func(content string) error {
			candidate, err := UnmarshalMetadataAssessments([]byte(content))
			if err != nil {
				return err
			}

			if _, err := request.ValidateAssessments(candidate); err != nil {
				logger.WarnContext(
					ctx,
					"The provider returned an invalid metadata assessment.",
					"schema_name",
					metadataAssessmentSchemaName,
					"finding_id",
					request.FindingID,
					"surface",
					request.Source.Surface,
					"err",
					err,
				)

				return fmt.Errorf("validate metadata assessment against current field: %w", err)
			}

			assessments = candidate

			return nil
		},
	)

	return assessments, err
}

// UnmarshalMetadataAssessments requires every structured decision field.
func UnmarshalMetadataAssessments(content []byte) ([]domain.MetadataFindingAssessment, error) {
	var response struct {
		Assessments []metadataAssessmentJSON `json:"assessments"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		slog.Warn("Metadata assessment decoding failed", "err", err)
		return nil, fmt.Errorf("decode metadata assessments: %w", err)
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("metadata assessment response must contain one JSON value")
	}

	if response.Assessments == nil {
		return nil, errors.New("metadata assessment response requires an assessments array")
	}

	candidate := make([]domain.MetadataFindingAssessment, 0, len(response.Assessments))
	for _, assessment := range response.Assessments {
		if assessment.FindingID == nil || assessment.Resolution == nil ||
			assessment.Reason == nil ||
			assessment.Evidence == nil ||
			assessment.StartLine == nil ||
			assessment.EndLine == nil {
			return nil, errors.New("metadata assessment response omits a required field")
		}

		candidate = append(
			candidate,
			domain.MetadataFindingAssessment{
				FindingID:  *assessment.FindingID,
				Resolution: *assessment.Resolution,
				Reason:     *assessment.Reason,
				Evidence:   *assessment.Evidence,
				StartLine:  *assessment.StartLine,
				EndLine:    *assessment.EndLine,
			},
		)
	}

	if err := domain.ValidateMetadataAssessments(candidate); err != nil {
		slog.Warn("Metadata assessment response validation failed", "err", err)
		return nil, fmt.Errorf("validate metadata assessments: %w", err)
	}

	return candidate, nil
}

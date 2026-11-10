package review

import (
	"context"

	"goodkind.io/pr-review-agent/internal/diff"
)

func (service *Service) reviewMetadataChunks(
	ctx context.Context,
	chunks []diff.Chunk,
	support string,
	settings reviewSettings,
	input diff.ReviewInput,
	record *metadataReviewRecord,
	seen map[string]struct{},
) error {
	for _, chunk := range chunks {
		var models modelSet

		requests := 0
		callContext, cancel := detachFromReviewDeadline(ctx, settings.chunkTimeout)
		analysis, reviewErr := reviewChunk(
			callContext,
			service.model,
			chunk,
			settings.minimumImportance,
			service.reviewPolicy,
			settings.maximumPromptBytes,
			nil,
			support,
			&models,
			&requests,
			service.now,
		)

		cancel()

		if reviewErr != nil {
			return reviewErr
		}

		if len(analysis.Unreadable) > 0 {
			return errMetadataChanged
		}

		for _, result := range analysis.Results {
			if !chunk.CoverageComplete && !result.OmissionsAcceptable {
				record.OmissionsAccepted = false
				record.DecisionReason = sanitizeDecisionReason(result.DecisionReason)
			}

			grounded := GroundedFindings(
				ctx,
				result.Findings,
				nil,
				BuildMetadataIndex(chunk.Metadata),
				chunk.Text,
			)

			eligible := EligibleFindings(
				grounded,
				nil,
				BuildMetadataIndex(input.Metadata),
				settings.minimumImportance,
			)
			for _, finding := range eligible {
				key := metadataFindingKey(finding)
				if _, found := seen[key]; found {
					continue
				}

				seen[key] = struct{}{}

				record.Findings = append(record.Findings, finding)
			}
		}
	}

	return nil
}

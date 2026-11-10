package review

import (
	"context"
	"fmt"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func (service *Service) reviewCurrentMetadata(
	ctx context.Context,
	job domain.ReviewJob,
	pullRequest githubapp.PullRequest,
	settings reviewSettings,
	prior []domain.Finding,
) (metadataReviewRecord, error) {
	var empty metadataReviewRecord

	input, err := service.collector.CollectRange(ctx, job.PullRequestRef, pullRequest, "")
	if err != nil {
		logger := gklog.L(ctx)
		logger.WarnContext(ctx, "Collect current pull request prose failed", "err", err)

		return empty, fmt.Errorf("collect current pull request prose: %w", err)
	}

	if input.Metadata == nil && pullRequest.CommitCount == 0 {
		input.Metadata = diff.PullRequestMetadata(pullRequest, nil)
	}

	metadataInput := diff.ReviewInput{
		PullRequest: pullRequest,
		Files:       nil,
		Metadata:    input.Metadata,
		MergeBase:   "",
	}

	support, err := BuildPullRequestContext(pullRequest, input.Files, service.reviewPolicy)
	if err != nil {
		gklog.L(ctx).WarnContext(ctx, "Build pull request prose context failed", "err", err)
		return empty, err
	}

	chunks, err := diff.ChunkInput(metadataInput, settings.maximumPromptBytes-len(support))
	if err != nil {
		gklog.L(ctx).WarnContext(ctx, "Split current pull request prose failed", "err", err)
		return empty, fmt.Errorf("split current pull request prose: %w", err)
	}

	chunks, support, err = service.boundMetadataChunks(metadataInput, chunks, support, settings)
	if err != nil {
		return empty, err
	}

	record := metadataReviewRecord{
		Head:       pullRequest.Head,
		Revision:   diff.PullRequestRevision(pullRequest),
		Findings:   nil,
		Checkpoint: nil,
		Omissions: classifyStructuralShortfall(deltaWork{
			AssessmentRequests: 0,
			FileAssessments:    nil,
			AssessmentModels:   nil,
			PullRequest:        pullRequest,
			Files:              nil,
			Metadata:           input.Metadata,
			Chunks:             chunks,
		}).Hunks,
		OmissionsAccepted: true,
		DecisionReason:    "",
	}
	seen := make(map[string]struct{})

	if err := service.reviewMetadataChunks(
		ctx,
		chunks,
		support,
		settings,
		input,
		&record,
		seen,
	); err != nil {
		return empty, err
	}

	retained, err := service.reassessMetadataFindings(
		ctx,
		job,
		prior,
		input.Metadata,
		pullRequest,
		settings,
	)
	if err != nil {
		return empty, err
	}

	for _, finding := range retained {
		key := metadataFindingKey(finding)
		if _, found := seen[key]; found {
			continue
		}

		seen[key] = struct{}{}

		record.Findings = append(record.Findings, finding)
	}

	return record, nil
}

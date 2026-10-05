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
		PullRequest: pullRequest, Files: nil, Metadata: input.Metadata, MergeBase: "",
	}
	support, err := pullRequestPrompt(pullRequest, input.Files, service.reviewPolicy)
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
		Head: pullRequest.Head, Revision: diff.PullRequestRevision(pullRequest), Findings: nil,
		Omissions:         classifyStructuralShortfall(deltaWork{PullRequest: pullRequest, Files: nil, Metadata: input.Metadata, Chunks: chunks}).Hunks,
		OmissionsAccepted: true, DecisionReason: "",
	}
	seen := make(map[string]struct{})
	for _, chunk := range chunks {
		var models modelSet
		requests := 0
		callContext, cancel := detachFromReviewDeadline(ctx, settings.chunkTimeout)
		analysis, reviewErr := reviewChunk(callContext, service.model, chunk, settings.minimumImportance, service.reviewPolicy, settings.maximumPromptBytes, nil, support, &models, &requests, service.now)
		cancel()
		if reviewErr != nil {
			return empty, reviewErr
		}
		if len(analysis.Unreadable) > 0 {
			return empty, errMetadataChanged
		}
		for _, result := range analysis.Results {
			if !chunk.CoverageComplete && !result.OmissionsAcceptable {
				record.OmissionsAccepted = false
				record.DecisionReason = sanitizeDecisionReason(result.DecisionReason)
			}
			grounded := GroundedFindings(ctx, result.Findings, nil, BuildMetadataIndex(chunk.Metadata), chunk.Text)
			eligible := EligibleFindings(grounded, nil, BuildMetadataIndex(input.Metadata), settings.minimumImportance)
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
	return record, nil
}

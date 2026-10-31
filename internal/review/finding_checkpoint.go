package review

import (
	"errors"
	"slices"

	"goodkind.io/pr-review-agent/internal/domain"
)

// findingCheckpoint distinguishes prior findings from completed results in a partial pass.
type findingCheckpoint struct {
	Prior []int `json:"prior,omitempty"`
}

func (checkpoint *findingCheckpoint) validate(findings []domain.Finding) error {
	if checkpoint == nil {
		return nil
	}
	seen := make(map[int]bool, len(checkpoint.Prior))
	for _, index := range checkpoint.Prior {
		if index < 0 || index >= len(findings) || seen[index] {
			return errors.New("the saved finding provenance is invalid")
		}
		seen[index] = true
	}
	return nil
}

func (checkpoint *findingCheckpoint) restore(findings []domain.Finding, reassess bool, completed []string) (current, prior []domain.Finding) {
	if reassess || checkpoint == nil && len(completed) == 0 {
		return nil, slices.Clone(findings)
	}
	for index, finding := range findings {
		if checkpoint != nil && slices.Contains(checkpoint.Prior, index) {
			prior = append(prior, finding)
		} else {
			current = append(current, finding)
		}
	}
	return current, prior
}

func (pass *chunkPass) restoreFindings(metadata metadataReviewRecord, fallback fallbackReviewRecord, reassess bool, completed []string) {
	pass.metadata, pass.retainedMetadata = metadata.Checkpoint.restore(metadata.Findings, reassess, completed)
	for _, finding := range pass.metadata {
		pass.metadataSeen[metadataFindingKey(finding)] = struct{}{}
	}
	pass.fallback, pass.retainedFallback = fallback.Checkpoint.restore(fallback.Findings, reassess, completed)
	pass.failed = len(pass.fallback)
	for _, finding := range pass.fallback {
		pass.selection.remember(finding)
	}
	if !reassess {
		for _, id := range completed {
			pass.reassessedChunks[id] = true
		}
	}
}

func (progress *reviewProgress) restoreFindings(metadata metadataReviewRecord, fallback fallbackReviewRecord) {
	progress.metadata = metadata.Findings
	progress.metadataCheckpoint = metadata.Checkpoint
	progress.metadataRevision = metadata.Revision
	progress.fallback = fallback.Findings
	progress.fallbackCheckpoint = fallback.Checkpoint
	if progress.metadataRevision == "" {
		progress.metadataRevision = fallback.Revision
	}
}

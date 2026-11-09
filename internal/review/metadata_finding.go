package review

import (
	"fmt"
	"slices"
	"strings"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func (pass *chunkPass) metadataSnapshot() ([]domain.Finding, *findingCheckpoint) {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	findings := slices.Clone(pass.metadata)
	checkpoint := &findingCheckpoint{Prior: nil}

	seen := make(map[string]struct{}, len(findings))
	for _, finding := range findings {
		seen[metadataFindingKey(finding)] = struct{}{}
	}

	for _, finding := range pass.retainedMetadata {
		key := metadataFindingKey(finding)
		if _, found := seen[key]; found {
			continue
		}

		seen[key] = struct{}{}
		checkpoint.Prior = append(checkpoint.Prior, len(findings))
		findings = append(findings, finding)
	}

	return findings, checkpoint
}

func metadataFindingKey(finding domain.Finding) string {
	if finding.Deduplication == reviewrules.DeduplicateLocation {
		return fmt.Sprintf(
			"%s:%s:%s:%d:%d",
			finding.EffectiveSurface(),
			finding.CommitSHA,
			finding.RuleID,
			finding.StartLine,
			finding.EndLine,
		)
	}

	evidence := strings.TrimSpace(finding.Evidence)
	if evidence == "" {
		evidence = finding.Title + "\n" + finding.Body + "\n" + finding.Suggestion
	}

	return fmt.Sprintf(
		"%s:%s:%s:%s",
		finding.EffectiveSurface(),
		finding.CommitSHA,
		finding.RuleID,
		evidence,
	)
}

// BuildMetadataIndex combines contiguous fragments using their original line numbers.
func BuildMetadataIndex(
	sources []diff.MetadataSource,
) map[domain.FindingTarget]diff.MetadataSource {
	index := make(map[domain.FindingTarget]diff.MetadataSource, len(sources))
	for _, source := range sources {
		if source.StartLine == 0 {
			source.StartLine = 1
		}

		if source.EndLine == 0 {
			source.EndLine = source.StartLine + len(strings.Split(source.Text, "\n")) - 1
		}

		key := source.Target()
		if previous, found := index[key]; found && previous.EndLine+1 == source.StartLine {
			source.Text = previous.Text + "\n" + source.Text
			source.StartLine = previous.StartLine
		}

		index[key] = source
	}

	return index
}

func metadataFindingAnchored(finding domain.Finding, source diff.MetadataSource) bool {
	if finding.Validate() != nil || finding.StartLine < source.StartLine ||
		finding.EndLine > source.EndLine {
		return false
	}

	lines := strings.Split(source.Text, "\n")
	start, end := finding.StartLine-source.StartLine, finding.EndLine-source.StartLine+1
	if start < 0 || end > len(lines) || start >= end || strings.TrimSpace(finding.Evidence) == "" {
		return false
	}

	if !matchesFileLine(strings.Join(lines[start:end], "\n"), finding.Evidence) {
		return false
	}

	if finding.EffectiveSurface() != domain.FindingCommitMessage {
		return true
	}

	for _, line := range lines[start:end] {
		if protectedCommitTrailer(line) &&
			(finding.CorrectionAction != domain.CorrectionReplace || !matchesFileLine(
				finding.Suggestion,
				line,
			)) {
			return false
		}
	}

	return true
}

func protectedCommitTrailer(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	return strings.HasPrefix(lower, "co-authored-by:") || strings.HasPrefix(lower, "signed-off-by:")
}

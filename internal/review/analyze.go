package review

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

// Analyze reviews every deterministic chunk and aggregates the model output.
func Analyze(ctx context.Context, model Model, input diff.ReviewInput, minimumImportance int) (Analysis, error) {
	chunks, err := diff.ChunkInput(input, config.MaximumPromptBytes)
	if err != nil {
		return Analysis{}, fmt.Errorf("chunk input: %w", err)
	}

	coverageComplete := inputCoverageComplete(input.Files)
	for _, chunk := range chunks {
		if !chunk.CoverageComplete {
			coverageComplete = false
			break
		}
	}

	fileIndex := buildFileIndex(input.Files)
	seen := make(map[string]struct{})
	anchored := make([]domain.Finding, 0)

	for _, chunk := range chunks {
		result, err := model.Review(ctx, buildPrompt(chunk, minimumImportance))
		if err != nil {
			return Analysis{}, fmt.Errorf("review chunk %d/%d: %w", chunk.Index, chunk.Total, err)
		}
		if err := result.Validate(); err != nil {
			slog.ErrorContext(ctx, "validate review result", slog.String("err", err.Error()))
			return Analysis{}, fmt.Errorf("validate review result: %w", err)
		}
		if !result.CoverageComplete {
			coverageComplete = false
		}

		for _, finding := range result.Findings {
			sanitized := sanitizeFinding(finding)
			if sanitized.Importance < minimumImportance {
				continue
			}
			normalizedPath, pathErr := marker.NormalizePath(sanitized.Path)
			if pathErr == nil {
				sanitized.Path = normalizedPath
			}

			key := normalizedFindingKey(sanitized)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}

			if isAnchored(sanitized, fileIndex) {
				anchored = append(anchored, sanitized)
			}
		}
	}

	sortFindings(anchored)

	return Analysis{
		CoverageComplete: coverageComplete,
		Anchored:         anchored,
		Decision:         DecisionFor(anchored, minimumImportance),
	}, nil
}

func buildPrompt(chunk diff.Chunk, minimumImportance int) string {
	var builder strings.Builder
	builder.WriteString("Review changed lines. Return only concrete defects with importance ")
	fmt.Fprintf(&builder, "%d", minimumImportance)
	builder.WriteString(" or higher. Return no findings when none meet that threshold. Review chunk ")
	fmt.Fprintf(&builder, "%d/%d", chunk.Index, chunk.Total)
	builder.WriteString(".\n")
	builder.WriteString(WrapUntrusted(chunk.Text))
	return builder.String()
}

func buildFileIndex(files []diff.FileContext) map[string]diff.FileContext {
	index := make(map[string]diff.FileContext, len(files))
	for _, file := range files {
		normalizedPath, err := marker.NormalizePath(file.Path)
		if err != nil {
			continue
		}
		index[normalizedPath] = file
	}
	return index
}

func inputCoverageComplete(files []diff.FileContext) bool {
	for _, file := range files {
		if !file.CoverageComplete {
			return false
		}
	}
	return true
}

func isAnchored(finding domain.Finding, fileIndex map[string]diff.FileContext) bool {
	normalizedPath, err := marker.NormalizePath(finding.Path)
	if err != nil {
		return false
	}
	file, ok := fileIndex[normalizedPath]
	if !ok {
		return false
	}
	if !diff.ValidRange(file.ChangedRightLines, file.ChangedRightHunks, finding.StartLine, finding.EndLine) {
		return false
	}
	return true
}

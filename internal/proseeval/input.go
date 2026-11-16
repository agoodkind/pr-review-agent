// Package proseeval builds source inputs for prose evaluation.
package proseeval

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

var (
	errInput        = errors.New("build evaluation input")
	errSourceFields = errors.New(
		"evaluation source files require path, status, patch, and current_content",
	)
	errSourceHash   = errors.New("evaluation source hash does not match current_content")
	errMetadataHash = errors.New("evaluation metadata hash does not match text")
)

// File contains the current file content and its patch.
type File = struct {
	Path           string `json:"path"`
	Status         string `json:"status"`
	Patch          string `json:"patch"`
	CurrentContent string `json:"current_content"`
	SourceSHA256   string `json:"source_sha256"`
}

// Metadata contains prose from a pull request or commit.
type Metadata = struct {
	Surface      domain.FindingSurface `json:"surface"`
	CommitSHA    domain.HeadSHA        `json:"commit_sha"`
	Text         string                `json:"text"`
	StartLine    int                   `json:"start_line"`
	EndLine      int                   `json:"end_line"`
	SourceSHA256 string                `json:"source_sha256"`
}

// BuildInput validates source content and constructs the review input.
//
// It checks optional source digests before preparing file and metadata changes.
//
// For example:
//
//	input, hashes, err := BuildInput(files, metadata)
func BuildInput[FileInput ~File, MetadataInput ~Metadata](
	files []FileInput,
	metadataSources []MetadataInput,
) (diff.ReviewInput, map[string]string, error) {
	var pullRequest githubapp.PullRequest
	input := diff.ReviewInput{
		PullRequest: pullRequest,
		Files:       nil,
		Metadata:    nil,
		MergeBase:   "",
	}
	hashes := make(map[string]string)

	for _, sourceFile := range files {
		file := File(sourceFile)
		if file.Path == "" || file.Patch == "" || file.CurrentContent == "" || file.Status == "" {
			return input, hashes, errSourceFields
		}

		hash := sourceSHA256([]byte(file.CurrentContent))
		if file.SourceSHA256 != "" && file.SourceSHA256 != hash {
			return input, hashes, errSourceHash
		}

		if err := diff.ValidatePatchContent(file.Patch, file.CurrentContent); err != nil {
			slog.Warn("Validate evaluation source failed", "err", err)

			return input, hashes, fmt.Errorf("%w: %w", errInput, err)
		}

		lines, hunks, err := diff.ChangedRightLines(file.Patch)
		if err != nil {
			slog.Warn("Read evaluation patch failed", "err", err)

			return input, hashes, fmt.Errorf("%w: %w", errInput, err)
		}

		hashes[file.Path] = hash
		input.Files = append(
			input.Files,
			diff.FileContext{
				Path:              file.Path,
				Status:            file.Status,
				Patch:             file.Patch,
				CurrentContent:    file.CurrentContent,
				Attributes:        nil,
				ChangedRightLines: lines,
				ChangedRightHunks: hunks,
				CoverageComplete:  true,
				Gap:               diff.CoverageGapNone,
			},
		)
	}

	for _, metadataSource := range metadataSources {
		metadata := Metadata(metadataSource)
		hash := sourceSHA256([]byte(metadata.Text))
		if metadata.SourceSHA256 != "" && metadata.SourceSHA256 != hash {
			return input, hashes, errMetadataHash
		}

		source := diff.MetadataSource{
			Surface:   metadata.Surface,
			CommitSHA: metadata.CommitSHA,
			Text:      metadata.Text,
			StartLine: metadata.StartLine,
			EndLine:   metadata.EndLine,
		}
		label, err := source.Label()
		if err != nil {
			slog.Warn("Render evaluation metadata label failed", "err", err)

			return input, hashes, fmt.Errorf("%w: %w", errInput, err)
		}

		hashes[label] = hash
		input.Metadata = append(input.Metadata, source)
	}

	return input, hashes, nil
}

func sourceSHA256(data []byte) string {
	hash := sha256.Sum256(data)

	return hex.EncodeToString(hash[:])
}

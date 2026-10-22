package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// ErrAssessmentIncomplete requires further review when a response cannot justify a file decision.
var ErrAssessmentIncomplete = errors.New("the file assessment requires current file decisions and supplied evidence")

// AssessFileContent assesses one unread file against the supplied parent pull request.
func AssessFileContent(ctx context.Context, model Model, pullRequest githubapp.PullRequest, files []diff.FileContext, path string, policy reviewrules.Policy, maximumBytes int) (Completion, error) {
	var empty Completion
	file, found := BuildFileIndex(files)[path]
	if !found {
		return empty, errors.New("file assessment requires a supplied changed file")
	}
	shortfall := structuralShortfall{Hunks: []unreadHunk{{Path: path, Header: "", Reason: fileGapReason(file.Gap), Target: nil}}}
	var data reviewrules.PromptData
	instruction, err := renderPrompt(policy, "omissions.decision", data)
	if err != nil {
		return empty, err
	}
	instruction += "\n" + policy.FileClassificationPathInstructions(path) + "\n"
	omissions, err := omissionPrompt(shortfall, files, maximumBytes, policy)
	if err != nil {
		return empty, err
	}
	parent, err := pullRequestPrompt(pullRequest, files, policy)
	if err != nil {
		return empty, err
	}
	prompt, err := BoundReviewPrompt(model, instruction+omissions, parent, maximumBytes)
	if err != nil {
		return empty, err
	}
	completion, err := model.Review(ctx, prompt)
	if err != nil {
		slog.ErrorContext(ctx, "assess file content", slog.String("path", path), slog.Any("err", err))
		return completion, fmt.Errorf("assess file content for %s: %w", path, err)
	}
	if err := completion.Result.Validate(); err != nil {
		return completion, fmt.Errorf("validate file assessment: %w", err)
	}
	for index := range completion.Result.FileAssessments {
		completion.Result.FileAssessments[index].Reviewed = false
	}
	validated, accepted := validateFileAssessments(completion.Result, shortfall, files, policy)
	if len(validated) != 1 {
		return completion, ErrAssessmentIncomplete
	}
	for _, reference := range validated[0].Evidence {
		marker := reference
		if reference == "pull_request" || reference == "changed_files" {
			marker = "Evidence ID: " + reference + "\n"
		}
		if !strings.Contains(prompt, marker) {
			return completion, fmt.Errorf("file assessment cited evidence outside the supplied request: %w", ErrAssessmentIncomplete)
		}
	}
	completion.Result.FileAssessments = validated
	completion.Result.OmissionsAcceptable = accepted
	return completion, nil
}

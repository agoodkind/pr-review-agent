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

// BuildFileAssessmentPrompt preserves supplied scope evidence within the actual provider budget.
func BuildFileAssessmentPrompt(model Model, pullRequest githubapp.PullRequest, files []diff.FileContext, path string, policy reviewrules.Policy, maximumBytes int) (string, error) {
	file, found := BuildFileIndex(files)[path]
	if !found {
		return "", errors.New("file assessment requires a supplied changed file")
	}
	shortfall := structuralShortfall{Hunks: []unreadHunk{{Path: path, Header: "", Reason: fileGapReason(file.Gap), Target: nil}}}
	var data reviewrules.PromptData
	instruction, err := renderPrompt(policy, "omissions.decision", data)
	if err != nil {
		return "", err
	}
	instruction += "\n" + policy.FileClassificationPathInstructions(path) + "\n"
	omissions, err := omissionPrompt(shortfall, files, maximumBytes, policy)
	if err != nil {
		return "", err
	}
	parent, err := pullRequestPrompt(pullRequest, files, policy)
	if err != nil {
		return "", err
	}
	return BoundReviewPrompt(model, instruction+omissions, parent, maximumBytes)
}

// AssessFileContent assesses one unread file against the supplied parent pull request.
func AssessFileContent(ctx context.Context, model Model, pullRequest githubapp.PullRequest, files []diff.FileContext, path string, policy reviewrules.Policy, maximumBytes int) (Completion, error) {
	var empty Completion
	prompt, err := BuildFileAssessmentPrompt(model, pullRequest, files, path, policy, maximumBytes)
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
	input := diff.ReviewInput{PullRequest: pullRequest, Files: files, Metadata: nil, MergeBase: ""}
	_, decisions, err := PlanFileContentReview(input, completion.Result.FileAssessments, policy)
	if err != nil {
		return completion, err
	}
	completion.Result.FileAssessments = decisions
	file := BuildFileIndex(files)[path]
	shortfall := structuralShortfall{Hunks: []unreadHunk{{Path: path, Header: "", Reason: fileGapReason(file.Gap), Target: nil}}}
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

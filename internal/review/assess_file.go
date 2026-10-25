package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// ErrAssessmentIncomplete requires further review when a response cannot justify a file decision.
var ErrAssessmentIncomplete = errors.New("the file assessment requires current file decisions and supplied evidence")

// FileContentClassifier separates scope decisions from ordinary finding requests.
type FileContentClassifier interface {
	ClassifyFileContent(context.Context, string) (Completion, error)
	ClassificationRequestBytes(string) (int, error)
}

// BuildFileAssessmentPrompt preserves supplied scope evidence within the actual provider budget.
func BuildFileAssessmentPrompt(model Model, pullRequest githubapp.PullRequest, files []diff.FileContext, path string, policy reviewrules.Policy, maximumBytes int, assessments ...[]domain.FileAssessment) (string, error) {
	file, found := BuildFileIndex(files)[path]
	if !found {
		return "", errors.New("file assessment requires a supplied changed file")
	}
	shortfall := structuralShortfall{Hunks: []unreadHunk{{Path: path, Header: "", Reason: fileGapReason(file.Gap), Target: nil}}}
	var data reviewrules.PromptData
	data.Path = path
	instruction, err := renderPrompt(policy, "omissions.decision", data)
	if err != nil {
		return "", err
	}
	instruction += "\n" + policy.FileClassificationPathInstructions(path) + "\n"
	omissions, err := omissionPrompt(shortfall, files, maximumBytes, policy, assessments...)
	if err != nil {
		return "", err
	}
	parent, err := pullRequestPrompt(pullRequest, files, policy)
	if err != nil {
		return "", err
	}
	if classifier, ok := model.(FileContentClassifier); ok {
		return boundReviewPrompt(classifier.ClassificationRequestBytes, instruction+omissions, parent, maximumBytes)
	}
	return BoundReviewPrompt(model, instruction+omissions, parent, maximumBytes)
}

// AssessFileContent assesses one unread file against the supplied parent pull request.
func AssessFileContent(ctx context.Context, model Model, pullRequest githubapp.PullRequest, files []diff.FileContext, path string, policy reviewrules.Policy, maximumBytes int, assessments ...[]domain.FileAssessment) (Completion, error) {
	var empty Completion
	prompt, err := BuildFileAssessmentPrompt(model, pullRequest, files, path, policy, maximumBytes, assessments...)
	if err != nil {
		return empty, err
	}
	var completion Completion
	if classifier, ok := model.(FileContentClassifier); ok {
		completion, err = classifier.ClassifyFileContent(ctx, prompt)
	} else {
		completion, err = model.Review(ctx, prompt)
	}
	if err != nil {
		if ctx.Err() == nil {
			err = normalizeFileAssessmentError(err)
		}
		slog.ErrorContext(ctx, "assess file content", slog.String("path", path), slog.Any("err", err))
		return completion, fmt.Errorf("assess file content for %s: %w", path, err)
	}
	if err := completion.Result.Validate(); err != nil {
		return completion, fmt.Errorf("validate file assessment: %w", normalizeFileAssessmentError(err))
	}
	if count := len(completion.Result.FileAssessments); count != 1 {
		code := AssessmentExtra
		if count == 0 {
			code = AssessmentMissing
		}
		return completion, assessmentValidationFailure(code, count, 1)
	}
	if completion.Result.FileAssessments[0].Path != path {
		return completion, assessmentValidationFailure(AssessmentTarget, 0, 1)
	}
	input := diff.ReviewInput{PullRequest: pullRequest, Files: files, Metadata: nil, MergeBase: ""}
	_, decisions, err := PlanFileContentReview(input, completion.Result.FileAssessments, policy)
	if err != nil {
		return completion, err
	}
	completion.Result.FileAssessments = decisions
	file := BuildFileIndex(files)[path]
	shortfall := structuralShortfall{Hunks: []unreadHunk{{Path: path, Header: "", Reason: fileGapReason(file.Gap), Target: nil}}}
	validated, accepted, err := validateFileAssessmentsDetailed(completion.Result, shortfall, files, policy)
	if err != nil {
		return completion, err
	}
	for _, reference := range validated[0].Evidence {
		marker := reference
		if reference == "pull_request" || reference == "changed_files" {
			marker = "Evidence ID: " + reference + "\n"
		}
		if !strings.Contains(prompt, marker) {
			return completion, assessmentValidationFailure(AssessmentUnsuppliedEvidence, 1, 0)
		}
	}
	completion.Result.FileAssessments = validated
	completion.Result.OmissionsAcceptable = accepted
	return completion, nil
}

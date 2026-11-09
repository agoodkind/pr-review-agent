package review

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

type admittedFiles struct {
	input           diff.ReviewInput
	deterministic   []domain.FileAssessment
	unresolvedFiles []diff.FileContext
}

type admissionPlan struct {
	work          deltaWork
	contentInput  diff.ReviewInput
	required      string
	promptContext string
}

func (service *Service) collectAdmissionInput(
	ctx context.Context,
	job domain.ReviewJob,
	pullRequest githubapp.PullRequest,
	checkRun githubapp.CheckRun,
	base domain.HeadSHA,
	progress *reviewProgress,
	settings reviewSettings,
	logger *slog.Logger,
) (admittedFiles, bool, error) {
	var empty admittedFiles
	input, err := service.collector.CollectRange(ctx, job.PullRequestRef, pullRequest, base)
	if err != nil {
		logger.ErrorContext(ctx, "collect pull request diff", slog.String("err", err.Error()))

		return empty, true, service.failCheck(
			ctx,
			job,
			checkRun.ID,
			progress.summary(service.now()),
			checkFailureDiff,
			fmt.Errorf("collect pull request diff: %w", err),
		)
	}

	progress.reached("the diff")

	fileAdmission, deterministic, unresolvedFiles, err := service.admitFileInput(input, settings)
	if err != nil {
		return empty, true, service.failCheck(
			ctx,
			job,
			checkRun.ID,
			progress.summary(service.now()),
			checkFailureAnalysis,
			err,
		)
	}

	if fileAdmission.Skip {
		if err := service.declineReview(ctx, job, checkRun.ID, fileAdmission); err != nil {
			return empty, true, service.failCheck(
				ctx,
				job,
				checkRun.ID,
				progress.summary(service.now()),
				checkFailureSkip,
				err,
			)
		}

		return empty, true, nil
	}

	return admittedFiles{
		input:           input,
		deterministic:   deterministic,
		unresolvedFiles: unresolvedFiles,
	}, false, nil
}

func (service *Service) prepareAdmissionPlan(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
	progress *reviewProgress,
	settings reviewSettings,
	logger *slog.Logger,
	admitted admittedFiles,
) (admissionPlan, bool, error) {
	var empty admissionPlan
	contentInput, assessments, assessmentModels, assessmentRequests, err := service.classifyInput(
		ctx,
		admitted.input,
		admitted.deterministic,
		admitted.unresolvedFiles,
		settings,
	)
	if err != nil {
		return empty, true, service.failCheck(
			ctx,
			job,
			checkRun.ID,
			progress.summary(service.now()),
			checkFailureAnalysis,
			err,
		)
	}

	work := deltaWork{
		AssessmentRequests: assessmentRequests,
		FileAssessments:    assessments,
		AssessmentModels:   assessmentModels,
		PullRequest:        admitted.input.PullRequest,
		Files:              admitted.input.Files,
		Metadata:           admitted.input.Metadata,
		Chunks:             nil,
	}

	promptContext, required, err := service.admissionContext(work)
	if err != nil {
		return empty, true, service.failCheck(
			ctx,
			job,
			checkRun.ID,
			progress.summary(service.now()),
			checkFailureAnalysis,
			err,
		)
	}

	chunks, err := diff.ChunkInput(contentInput, settings.maximumPromptBytes)
	if err != nil {
		logger.ErrorContext(ctx, "chunk input", slog.String("err", err.Error()))

		return empty, true, service.failCheck(
			ctx,
			job,
			checkRun.ID,
			progress.summary(service.now()),
			checkFailureDiff,
			fmt.Errorf("chunk input: %w", err),
		)
	}

	work.Chunks = chunks

	return admissionPlan{
		work:          work,
		contentInput:  contentInput,
		required:      required,
		promptContext: promptContext,
	}, false, nil
}

func (service *Service) admissionContext(work deltaWork) (string, string, error) {
	shortfall := classifyStructuralShortfall(work)
	promptContext, err := BuildPullRequestContext(
		work.PullRequest,
		work.Files,
		service.reviewPolicy,
	)
	if err != nil {
		return "", "", err
	}

	required, err := sourceScopePrompt(shortfall, service.reviewPolicy)
	if err != nil {
		return "", "", err
	}

	return promptContext, required, nil
}

func (service *Service) boundAdmissionChunks(
	plan admissionPlan,
	settings reviewSettings,
) (deltaWork, error) {
	work := plan.work
	required := plan.required
	promptContext := plan.promptContext
	var err error
	work.Chunks, err = BoundModelReviewChunksWithContext(
		service.model,
		work.Chunks,
		work.Files,
		settings.minimumImportance,
		required,
		promptContext,
		service.reviewPolicy,
		settings.maximumPromptBytes,
	)
	if err != nil {
		return work, err
	}

	shortfall := classifyStructuralShortfall(work)
	if shortfall.present() {
		required, err = sourceScopePrompt(shortfall, service.reviewPolicy)
		if err != nil {
			return work, err
		}

		work.Chunks, err = BoundModelReviewChunksWithContext(
			service.model,
			work.Chunks,
			work.Files,
			settings.minimumImportance,
			required,
			promptContext,
			service.reviewPolicy,
			settings.maximumPromptBytes,
		)
		if err != nil {
			return work, err
		}
	}

	work.Chunks, err = PrepareRelationshipChunks(
		service.model,
		work.Chunks,
		work.Files,
		settings.minimumImportance,
		required,
		promptContext,
		service.reviewPolicy,
		settings.maximumPromptBytes,
	)
	if err != nil {
		return work, err
	}

	return work, nil
}

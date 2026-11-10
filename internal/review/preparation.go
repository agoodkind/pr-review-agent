package review

import (
	"context"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

type owedPreparation struct {
	state         *marker.State
	hasState      bool
	fromScratch   bool
	policyChanged bool
}

func (service *Service) prepareOwedPass(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
	pullRequest githubapp.PullRequest,
	reviews []githubapp.Review,
	work deltaWork,
	startedAt time.Time,
	progress *reviewProgress,
	settings reviewSettings,
	preparation owedPreparation,
) (*chunkPass, error) {
	head := job.Head

	var (
		err            error
		metadataRecord metadataReviewRecord
		fallbackRecord fallbackReviewRecord
	)
	if preparation.hasState {
		metadataRecord, fallbackRecord, preparation.state.Completed, err = service.restoreFindingCheckpoints(
			ctx,
			job,
			pullRequest,
			work.Chunks,
			preparation.state.Completed,
		)
		if err != nil {
			return nil, service.failCheck(
				ctx,
				job,
				checkRun.ID,
				progress.summary(service.now()),
				checkFailureSummary,
				err,
			)
		}
	}

	progress.restoreFindings(metadataRecord, fallbackRecord)
	progress.fileAssessments = work.FileAssessments
	progress.models = work.AssessmentModels
	started := progress.summary(service.now())
	started.Failed = false
	started.InProgress = true
	service.announceStart(ctx, job, started)

	threads, err := service.reconcileThreads(ctx, job, checkRun.ID, progress)
	if err != nil {
		return nil, err
	}

	// Both are built from the threads reconciliation already loaded, so raising
	// an answered claim again costs the run no extra read and no extra model
	// call.
	selection := collectPublicationState(reviews, threads, service.botLogin)
	disputes := collectDisputes(threads, service.botLogin, head)

	disputePrompt, err := disputes.promptSection(service.reviewPolicy)
	if err != nil {
		return nil, service.failCheck(
			ctx,
			job,
			checkRun.ID,
			progress.summary(service.now()),
			checkFailureAnalysis,
			err,
		)
	}

	pass := newChunkPass(work, settings, &selection, disputes, disputePrompt, startedAt)
	pass.seedFileAssessments(service.reviewPolicy)
	pass.restoreFindings(
		metadataRecord,
		fallbackRecord,
		preparation.fromScratch || preparation.policyChanged,
		preparation.state.Completed,
	)

	return pass, nil
}

package review

import (
	"context"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

type publicationInputs struct {
	analysis           Analysis
	published          []domain.Finding
	metadata           []domain.Finding
	metadataCheckpoint *findingCheckpoint
	fallbackCheckpoint *findingCheckpoint
	failed             int
	fallback           []domain.Finding
	threads            []githubapp.ReviewThread
}

type publicationDecision struct {
	summary          Summary
	shortfall        structuralShortfall
	omissionsDecided bool
}

func (service *Service) loadPublicationInputs(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
	pass *chunkPass,
	progress *reviewProgress,
) (*publicationInputs, error) {
	// Findings either reached the diff or were retained for the summary, so the
	// review submitted here carries the verdict alone.
	analysis := pass.analysis()
	published := pass.publishedFindings()
	metadata, metadataCheckpoint := pass.metadataSnapshot()
	_, fallbackCheckpoint := pass.fallbackSnapshot()
	posted, failed, fallback := pass.delivery()
	logPublishedFindings(ctx, analysis.Anchored, published, posted, failed)
	progress.reached("finding selection")

	threads, err := service.openThreads(ctx, job, progress)
	if err != nil {
		return nil, service.failCheck(
			ctx,
			job,
			checkRun.ID,
			progress.summary(service.now()),
			checkFailureThreads,
			err,
		)
	}

	return &publicationInputs{
		analysis:           analysis,
		published:          published,
		metadata:           metadata,
		metadataCheckpoint: metadataCheckpoint,
		fallbackCheckpoint: fallbackCheckpoint,
		failed:             failed,
		fallback:           fallback,
		threads:            threads,
	}, nil
}

func (service *Service) decidePublication(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	reviews []githubapp.Review,
	pass *chunkPass,
	state marker.State,
	startedAt time.Time,
	settings reviewSettings,
	inputs *publicationInputs,
) publicationDecision {
	shortfall := pass.structuralShortfall()
	omissionsDecided := shortfall.present() && len(state.Pending) == 0 && len(state.Unread) == 0 &&
		pass.decidedOmissions()
	omissionsAccepted := omissionsDecided && pass.acceptsOmissions()
	// Completing the read does not establish approval when omissions were
	// refused or a finding could not reach the diff.
	headFullyReviewed := len(state.Pending) == 0 && len(state.Unread) == 0 &&
		(inputs.analysis.CoverageComplete || omissionsDecided) && inputs.failed == len(
		inputs.fallback,
	)
	approvalAllowed := headFullyReviewed && (!omissionsDecided || omissionsAccepted) &&
		len(inputs.fallback) == 0

	decision := reviewerDecision(inputs.threads, service.botLogin, approvalAllowed)
	if len(inputs.metadata) > 0 {
		decision = domain.ReviewDecisionRequestChanges
	}

	blocking := blockingReasons(inputs.threads, service.botLogin, job.PullRequestRef)

	summary := Summary{
		FileAssessmentNote: "",
		Head:               head,
		Decision:           decision,
		DecisionReason:     pass.decisionReason(),
		ApprovalWithheld:   !approvalAllowed,
		Report: Report{
			Summary:     "",
			Walkthrough: nil,
		},
		Blocking:                   blocking,
		Models:                     inputs.analysis.Models,
		Usage:                      UsageFromContext(ctx),
		Duration:                   service.now().Sub(startedAt),
		FilesReviewed:              inputs.analysis.FilesReviewed,
		Chunks:                     inputs.analysis.Chunks,
		CoverageComplete:           inputs.analysis.CoverageComplete,
		MinimumImportance:          settings.minimumImportance,
		Observed:                   inputs.analysis.Observed,
		Eligible:                   inputs.analysis.Anchored,
		Published:                  inputs.published,
		Fallback:                   inputs.fallback,
		Metadata:                   inputs.metadata,
		metadataCheckpoint:         inputs.metadataCheckpoint,
		fallbackCheckpoint:         inputs.fallbackCheckpoint,
		MetadataRevision:           diff.PullRequestRevision(pass.work.PullRequest),
		MetadataFindingsDisplay:    service.reviewPolicy.MetadataFindingsDisplay(),
		PriorReviews:               traceReviews(reviews, service.botLogin),
		Threads:                    traceThreads(inputs.threads, service.botLogin),
		Reached:                    "",
		Failed:                     false,
		InProgress:                 false,
		Forced:                     job.Forced,
		Omissions:                  nil,
		OmissionsAccepted:          omissionsAccepted,
		FileAssessments:            pass.fileAssessments(),
		FileAssessmentsDisplay:     service.reviewPolicy.FileAssessmentsDisplay(),
		FileAssessmentMaximumBytes: service.reviewPolicy.FileAssessmentMaximumBytes(),
	}
	if omissionsDecided {
		summary.Omissions = shortfall.Hunks
	}

	return publicationDecision{
		summary:          summary,
		shortfall:        shortfall,
		omissionsDecided: omissionsDecided,
	}
}

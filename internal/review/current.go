package review

import (
	"context"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func (service *Service) refreshMetadataRecord(
	ctx context.Context,
	job domain.ReviewJob,
	current githubapp.PullRequest,
	settings reviewSettings,
	assessment assessmentSnapshot,
) (metadataReviewRecord, bool, error) {
	var err error
	metadata := assessment.metadata
	metadataReassessed := false
	recorded := assessment.metadataRecorded
	if assessment.metadataError != nil {
		return metadata, metadataReassessed, assessment.metadataError
	}

	if !recorded {
		metadata, recorded, err = service.readMetadataRecord(ctx, job)
		if err != nil {
			return metadata, metadataReassessed, err
		}
	}

	if !recorded || metadata.Head != job.Head ||
		metadata.Revision != diff.PullRequestRevision(current) {
		metadata, err = service.reviewCurrentMetadata(
			ctx,
			job,
			current,
			settings,
			metadata.Findings,
		)
		if err != nil {
			return metadata, metadataReassessed, err
		}

		metadataReassessed = true
	}

	return metadata, metadataReassessed, nil
}

func (service *Service) planRefreshedVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	inputs verdictRefreshInputs,
	metadata metadataReviewRecord,
	metadataReassessed bool,
	settings reviewSettings,
) (refreshedVerdict, error) {
	var empty refreshedVerdict
	// How much of the head was reviewed is recovered from the review that named
	// this head, because that is the run that knew. What the pull request
	// currently shows is a separate question, answered by the newest verdict
	// whatever head it named. A thread resolution changes neither.
	//
	// The durable checkpoint outranks that body wherever it speaks to this head.
	//
	// A read that failed decides nothing and stops the refresh here. Answering
	// true would approve a head whose incompleteness is recorded only in the
	// comment this call could not read, which is the one way this path opens a
	// gate over code nobody covered.
	headFullyReviewed, err := service.headReadWhole(ctx, job, inputs.assessment.legacyWholeHead)
	if err != nil {
		return empty, err
	}

	blockWithdrawn := inputs.blockWithdrawn
	omissions := inputs.assessment.omissions
	omissionsAccepted := inputs.assessment.omissionsAccepted
	decisionReason := inputs.assessment.decisionReason
	omissions, omissionsAccepted, decisionReason = updateMetadataOmissions(
		omissions,
		omissionsAccepted,
		decisionReason,
		metadata,
	)
	if len(omissions) > 0 && !omissionsAccepted {
		if decisionReason == "" {
			decisionReason = "The unread changes listed below need review before approval."
		}
	}

	approvalAllowed := headFullyReviewed && (len(omissions) == 0 || omissionsAccepted) &&
		!inputs.assessment.approvalWithheld
	decision := reviewerDecision(inputs.threads, service.botLogin, approvalAllowed)
	if len(metadata.Findings) > 0 {
		decision = domain.ReviewDecisionRequestChanges
	}

	return refreshedVerdict{
		standingReview:     inputs.standingReview,
		fileAssessments:    inputs.assessment.fileAssessments,
		decision:           decision,
		decisionReason:     decisionReason,
		approvalWithheld:   !approvalAllowed,
		standingState:      inputs.standingState,
		currentApproval:    inputs.verdict.State == reviewStateApproved,
		threads:            inputs.threads,
		headFullyReviewed:  headFullyReviewed,
		blockWithdrawn:     blockWithdrawn,
		settings:           settings,
		omissions:          omissions,
		omissionsAccepted:  omissionsAccepted,
		metadata:           metadata.Findings,
		metadataRevision:   metadata.Revision,
		metadataReassessed: metadataReassessed,
	}, nil
}

func (service *Service) refreshedSummary(
	ctx context.Context,
	job domain.ReviewJob,
	refreshed refreshedVerdict,
) Summary {
	return Summary{
		FileAssessmentNote:         "",
		Head:                       job.Head,
		FileAssessmentMaximumBytes: service.reviewPolicy.FileAssessmentMaximumBytes(),
		FileAssessments:            refreshed.fileAssessments,
		FileAssessmentsDisplay:     service.reviewPolicy.FileAssessmentsDisplay(),
		Decision:                   refreshed.decision,
		DecisionReason:             refreshed.decisionReason,
		ApprovalWithheld:           refreshed.approvalWithheld,
		Report: Report{
			Summary:     "",
			Walkthrough: nil,
		},
		Blocking: blockingReasons(
			refreshed.threads,
			service.botLogin,
			job.PullRequestRef,
		),
		Models:                  nil,
		Usage:                   UsageFromContext(ctx),
		Duration:                0,
		FilesReviewed:           0,
		Chunks:                  0,
		CoverageComplete:        refreshed.headFullyReviewed && len(refreshed.omissions) == 0,
		MinimumImportance:       refreshed.settings.minimumImportance,
		Observed:                nil,
		Eligible:                nil,
		Published:               nil,
		Fallback:                nil,
		Metadata:                refreshed.metadata,
		metadataCheckpoint:      nil,
		fallbackCheckpoint:      nil,
		MetadataRevision:        refreshed.metadataRevision,
		MetadataFindingsDisplay: service.reviewPolicy.MetadataFindingsDisplay(),
		Omissions:               refreshed.omissions,
		OmissionsAccepted:       refreshed.omissionsAccepted,
		PriorReviews:            nil,
		Threads:                 traceThreads(refreshed.threads, service.botLogin),
		Reached:                 "",
		Failed:                  false,
		InProgress:              false,
		// A refresh runs only at a head some earlier run already reviewed, which
		// is a gate a forced run never reaches.
		Forced: false,
	}
}

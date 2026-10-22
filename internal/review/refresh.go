package review

// This file reconciles the standing verdict with thread state at a head that is
// already reviewed.
//
// It is what lets resolving a thread unblock a pull request with no push. A
// pull_request_review_thread delivery arrives at a head some earlier run
// already judged, so there is no delta to review and nothing to ask a model:
// the verdict is a pure function of the service's own open threads and how much
// of the head was read, and both are already known.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// emptyReview is the zero verdict returned beside a failure or a miss.
func emptyReview() githubapp.Review {
	return githubapp.Review{ID: 0, CommitID: "", Author: "", Body: "", State: ""}
}

func (service *Service) reconcileReplyAndRefreshVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
	settings reviewSettings,
) (bool, error) {
	logger := gklog.L(ctx)
	var threads []githubapp.ReviewThread
	if job.ThreadRootCommentID != 0 {
		var err error
		threads, err = service.reconciler.Reconcile(ctx, job)
		if err != nil {
			logger.ErrorContext(ctx, "reconcile replied thread", slog.String("err", err.Error()))
			return false, fmt.Errorf("reconcile replied thread: %w", err)
		}
	}
	return service.refreshVerdictAtReviewedHead(ctx, job, reviews, threads, settings)
}

// refreshVerdictAtReviewedHead reconciles the standing verdict with current
// thread state at a head that is already reviewed.
//
// Every failure travels back to the caller rather than being logged and dropped
// here. A refresh that failed silently was indistinguishable from one that found
// nothing to do, so a resolved thread could keep blocking the pull request with
// nothing saying why.
//
// Returning it does not by itself retry anything: the webhook is acknowledged
// before this runs, and the dispatcher logs a failed job rather than replaying
// it. What this buys is that the failure is attributed to the run and carries
// its cause. The caller keeps withheld approval nonpassing and reports a failed
// refresh on the check without changing earlier review objects.
//
// reviews is the review list the caller already loaded, or nil when the run
// exited before loading one.
func (service *Service) refreshVerdictAtReviewedHead(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
	threads []githubapp.ReviewThread,
	settings reviewSettings,
) (bool, error) {
	ctx, cancel := service.publicationContext(ctx)
	defer cancel()
	logger := gklog.L(ctx)

	inputs, err := service.loadVerdictRefreshInputs(ctx, job, reviews, threads)
	if err != nil || !inputs.found {
		return false, err
	}
	state, hasState, err := service.readState(ctx, job)
	if err != nil {
		logger.ErrorContext(
			ctx,
			"read durable state before the verdict refresh",
			slog.String("err", err.Error()),
		)
		return false, fmt.Errorf("read durable state before the verdict refresh: %w", err)
	}
	if hasState && state.Status == marker.StateReviewing {
		logger.InfoContext(
			ctx,
			"verdict refresh skipped",
			slog.String("reason", "review_not_complete"),
		)
		return true, nil
	}
	current, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return false, fmt.Errorf("read current pull request prose: %w", err)
	}
	if current.Head != job.Head {
		return true, nil
	}
	if hasState && ((state.PolicyRevision != "" && bindCheckpointPolicy(state, current, service.reviewPolicy, settings).PolicyRevision != state.PolicyRevision) ||
		(state.PolicyRevision == "" && len(inputs.assessment.omissions) > 0)) {
		return false, errReviewContextChanged
	}
	metadata := inputs.assessment.metadata
	recorded := inputs.assessment.metadataRecorded
	if inputs.assessment.metadataError != nil {
		return false, inputs.assessment.metadataError
	}
	if !recorded {
		metadata, recorded, err = service.readMetadataRecord(ctx, job)
		if err != nil {
			return false, err
		}
	}
	if !recorded || metadata.Head != job.Head || metadata.Revision != diff.PullRequestRevision(current) {
		metadata, err = service.reviewCurrentMetadata(ctx, job, current, settings)
		if err != nil {
			return false, err
		}
	}
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
		return false, err
	}
	blockWithdrawn := inputs.blockWithdrawn
	omissions := inputs.assessment.omissions
	omissionsAccepted := inputs.assessment.omissionsAccepted
	decisionReason := inputs.assessment.decisionReason
	omissions, omissionsAccepted, decisionReason = updateMetadataOmissions(omissions, omissionsAccepted, decisionReason, metadata)
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
	err = service.applyRefreshedVerdict(ctx, job, refreshedVerdict{
		decision:          decision,
		decisionReason:    decisionReason,
		approvalWithheld:  !approvalAllowed,
		standingState:     inputs.standingState,
		currentApproval:   inputs.verdict.State == reviewStateApproved,
		threads:           inputs.threads,
		headFullyReviewed: headFullyReviewed,
		blockWithdrawn:    blockWithdrawn,
		settings:          settings,
		omissions:         omissions,
		omissionsAccepted: omissionsAccepted,
		metadata:          metadata.Findings,
		metadataRevision:  metadata.Revision,
	})
	return decision == domain.ReviewDecisionComment, err
}

func (service *Service) refreshReviewedCheck(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
	settings reviewSettings,
) error {
	logger := gklog.L(ctx)
	withheld, err := service.reconcileReplyAndRefreshVerdict(ctx, job, reviews, settings)
	if errors.Is(err, errReviewContextChanged) {
		return service.reassessChangedContext(ctx, job, reviews, settings)
	}
	if err != nil {
		logger.ErrorContext(ctx, "refresh reviewed check", slog.String("err", err.Error()))
		var statuses []ProviderStatus
		collectProviderStatuses(err, &statuses)
		recordAssessment(ctx, domain.AssessmentFailed, nil, failureNames(err), quotaRecovery(statuses))
		dismissErr := service.dismissFailureVerdict(ctx, job, err)
		detail := service.terminalFailureDetail(ctx, job, err)
		completionErr := service.completeCheckRun(ctx, job.InstallationID, job.Repository, job.CheckRunID,
			"failure", "Review refresh failed", detail)
		var progress Summary
		progress.Head = job.Head
		progress.Failed = true
		service.writeFailureSummary(ctx, job, progress, "Review refresh failed", detail)
		if completionErr == nil {
			return &reportedFailureError{cause: errors.Join(err, dismissErr)}
		}
		return errors.Join(err, completionErr, dismissErr)
	}
	if withheld {
		recordAssessment(ctx, domain.AssessmentDeclined, nil, nil, 0)
		return service.completeCheckRun(ctx, job.InstallationID, job.Repository, job.CheckRunID,
			checkConclusionDeclined, "Review needs attention", "Approval remains withheld. The review summary explains what needs attention.")
	}
	return service.succeed(ctx, job, job.CheckRunID, checkTitleAlreadyReviewed,
		"This head already has a PR-Agent review. No duplicate review was published.")
}

// headReadWhole reports whether any completed run read this whole head.
//
// A checkpoint naming this head answers it outright, because the baseline
// advances when and only when a run read the whole head with nothing left
// pending. That is the direct record, and it is right where the verdict body is
// wrong: every head blocked by the coverage the model used to be asked to answer
// blind carries the unreviewed sentence over a checkpoint recording the head as
// read whole, so believing the body there keeps a block standing on a fact that
// was never true.
//
// A checkpoint naming some other commit says nothing about this head, so the
// body decides. That case is a force push back to a commit already reviewed: the
// baseline sits on the commit in between, this head still carries the verdict
// the run that read it submitted, and treating an unrelated checkpoint as the
// answer would replace a valid approval with a block nobody can clear. The same
// holds for a pull request carrying no checkpoint at all.
//
// A read that failed is not an absence and is reported rather than guessed. The
// caller stops there, because a run that could not read the whole head submits
// no verdict, so its incompleteness exists only in the comment this call just
// failed to read.
func (service *Service) headReadWhole(
	ctx context.Context,
	job domain.ReviewJob,
	legacyWholeHead bool,
) (bool, error) {
	logger := gklog.L(ctx)
	state, hasState, err := service.readState(ctx, job)
	if err != nil {
		logger.ErrorContext(
			ctx,
			"read durable state for the verdict refresh",
			slog.String("err", err.Error()),
		)
		return false, fmt.Errorf("read durable state for the verdict refresh: %w", err)
	}
	// Pending work beside this baseline belongs to a later commit.
	if hasState && state.LastReviewed == job.Head {
		return true, nil
	}
	return legacyWholeHead, nil
}

// verdictRefreshInputs is everything a refresh decides from.
type verdictRefreshInputs struct {
	assessment assessmentSnapshot
	// Legacy native bodies provide assessments when the completed summary is absent.
	verdict githubapp.Review
	// standingState is what GitHub shows for this service now, across all heads.
	standingState string
	threads       []githubapp.ReviewThread
	found         bool
	// Native dismissal evidence distinguishes human withdrawals from failure recovery.
	blockWithdrawn bool
}

// loadVerdictRefreshInputs reads the standing verdict and the current threads a
// refresh decides from. It reports found false, with no error, when this head
// carries no verdict of the service's own to reconcile.
func (service *Service) loadVerdictRefreshInputs(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
	threads []githubapp.ReviewThread,
) (verdictRefreshInputs, error) {
	logger := gklog.L(ctx)
	var emptyAssessment assessmentSnapshot
	missing := verdictRefreshInputs{
		assessment: emptyAssessment, verdict: emptyReview(), standingState: "", threads: nil, found: false, blockWithdrawn: false,
	}
	if reviews == nil {
		listed, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
		if err != nil {
			logger.ErrorContext(ctx, "list reviews for verdict refresh", slog.String("err", err.Error()))
			return missing, fmt.Errorf("list reviews for verdict refresh: %w", err)
		}
		reviews = listed
	}
	verdict := latestBotVerdictAtHead(reviews, service.botLogin, job.Head)
	blockWithdrawn := verdict.withdrawn && dismissedVerdictBlocked(verdict.review.Body)
	if verdict.withdrawn && (blockWithdrawn || verdict.review.Body == "") {
		dismissal, found, err := service.github.FindReviewDismissal(ctx, job.InstallationID, job.Repository, job.Number, verdict.review.ID)
		if err != nil {
			logger.WarnContext(ctx, "read failure dismissal provenance", slog.String("err", err.Error()))
			return missing, fmt.Errorf("read failure dismissal provenance: %w", err)
		}
		if !found {
			return missing, errors.New("review dismissal event is not available")
		}
		blocked, err := dismissedReviewBlocked(dismissal, verdict.review.Body)
		if err != nil {
			logger.WarnContext(ctx, "read original dismissed decision", slog.String("err", err.Error()))
			return missing, err
		}
		blockWithdrawn = blocked && !automaticFailureDismissal(dismissal, service.botLogin, verdict.review.ID)
	}
	assessment, found, err := service.completedSummaryAssessment(ctx, job)
	if err != nil {
		return missing, err
	}
	if !found {
		assessment = parseAssessmentSnapshot(verdict.review.Body)
	}
	if !verdict.found && !found {
		return missing, nil
	}
	if threads == nil {
		listed, err := service.github.ListReviewThreads(ctx, job.InstallationID, job.Repository, job.Number)
		if err != nil {
			logger.ErrorContext(ctx, "list threads for verdict refresh", slog.String("err", err.Error()))
			return missing, fmt.Errorf("list threads for verdict refresh: %w", err)
		}
		threads = listed
	}
	return verdictRefreshInputs{
		assessment:     assessment,
		verdict:        verdict.review,
		standingState:  latestBotVerdictState(reviews, service.botLogin),
		threads:        threads,
		found:          true,
		blockWithdrawn: blockWithdrawn,
	}, nil
}

// refreshedVerdict is what one refresh computed from current thread state.
type refreshedVerdict struct {
	currentApproval   bool
	approvalWithheld  bool
	decision          domain.ReviewDecision
	decisionReason    string
	standingState     string
	threads           []githubapp.ReviewThread
	headFullyReviewed bool
	// blockWithdrawn is whether a person dismissed the verdict at this head,
	// which bounds what the refresh may submit rather than what it computes.
	blockWithdrawn bool
	// settings are the values this delivery is bound by. The refresh runs no
	// model call, so only the reported threshold comes from here, and it comes
	// from here rather than from the service so the summary a refresh writes and
	// the summary a review writes cannot disagree about it.
	settings          reviewSettings
	omissions         []unreadHunk
	omissionsAccepted bool
	metadata          []domain.Finding
	metadataRevision  string
}

// mayPublish reports whether the refresh may submit the verdict it computed.
//
// A refresh never reinstates a block a person withdrew. Dismissing the block is
// the operator's routine escape from it, and a service that restated the same
// block from thread state alone, seconds later and with nothing new learned,
// would make that escape useless and the block permanent, which is the failure
// this project exists to remove.
//
// This overrides the earlier rule, which required the block be restated so that
// a dismissal could not leave the pull request carrying no verdict. That reason
// is true as far as it goes and is outweighed. A dismissal does not discard the
// finding: the check run is the enforcement point, the open threads stay visible
// on the pull request, and a branch rule requiring threads to be resolved still
// holds. What it discards is one review object saying so.
//
// TestADismissedBlockIsNotRestatedButStillApprovesWhenThreadsResolve is the test
// this reversed. It asserted that an open thread at a dismissed head restates
// the block, and now asserts that nothing is submitted there.
//
// Only a dismissed block is withheld from. Dismissing an approval is the
// opposite request, a person saying they do not accept it and want more
// scrutiny, so withholding a later block would give them less; that head behaves
// normally and blocks when the recomputed decision blocks.
//
// The gate keys on the head, so it relaxes nothing beyond the commit somebody
// actually ruled on. A new head carries no dismissed verdict of its own and gets
// a fresh one from the run that reviews it.
//
// Approving is still allowed at a withheld head, and is the reason such a head
// is refreshed at all. Once every thread is resolved the recomputed verdict is
// an approval, which is what the person was reaching for, and the refresh is the
// only path that reaches it without a push.
//
// Otherwise a verdict matching what GitHub already shows is not submitted,
// because a second identical verdict is noise.
func (refreshed refreshedVerdict) mayPublish() bool {
	if refreshed.blockWithdrawn && refreshed.decision != domain.ReviewDecisionApprove {
		return false
	}
	if refreshed.decision == domain.ReviewDecisionApprove && refreshed.settings.approveEachCommit && !refreshed.currentApproval {
		return true
	}
	return reviewStateFor(refreshed.decision) != refreshed.standingState
}

// applyRefreshedVerdict writes what the refresh computed: a fresh verdict review
// when the decision moved, and the summary comment prose either way.
//
// The summary is rewritten even when the decision did not move. The blocking
// list carries one entry per open thread, so resolving one of several leaves the
// verdict identical while the list still names the thread that just closed, and
// a reader acting on that list goes looking for something already dealt with.
// The durable state is kept exactly as the last completed run wrote it.
func (service *Service) applyRefreshedVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	refreshed refreshedVerdict,
) error {
	logger := gklog.L(ctx)
	// A verdict written at a moved head would judge a commit nobody is looking
	// at any more, so the refresh checks the head one last time.
	currentPullRequest, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "refresh verdict head check", slog.String("err", err.Error()))
		return fmt.Errorf("refresh verdict head check: %w", err)
	}
	if currentPullRequest.Head != job.Head {
		logger.InfoContext(ctx, "verdict refresh skipped", slog.String("reason", "head_moved"))
		return nil
	}
	if diff.PullRequestRevision(currentPullRequest) != refreshed.metadataRevision {
		return errMetadataChanged
	}

	blocking := blockingReasons(
		refreshed.threads, service.botLogin, job.PullRequestRef,
	)
	summary := Summary{
		FileAssessmentMaximumBytes: service.reviewPolicy.FileAssessmentMaximumBytes(),
		FileAssessments:            nil,
		FileAssessmentsDisplay:     service.reviewPolicy.FileAssessmentsDisplay(),
		Head:                       job.Head,
		Decision:                   refreshed.decision,
		DecisionReason:             refreshed.decisionReason,
		ApprovalWithheld:           refreshed.approvalWithheld,
		Report: Report{
			Summary:     "",
			Walkthrough: nil,
		},
		Blocking:                blocking,
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
		MetadataRevision:        refreshed.metadataRevision,
		MetadataFindingsDisplay: service.reviewPolicy.MetadataFindingsDisplay(),
		Omissions:               refreshed.omissions,
		OmissionsAccepted:       refreshed.omissionsAccepted,
		PriorReviews:            nil,
		Threads:                 traceThreads(refreshed.threads, service.botLogin),
		Reached:                 "",
		Failed:                  false,
		// A refresh runs only at a head some earlier run already reviewed, which
		// is a gate a forced run never reaches.
		Forced: false,
	}
	if !refreshed.mayPublish() && !refreshed.blockWithdrawn {
		if err := service.validateInlineVerdict(ctx, job, summary); err != nil {
			return err
		}
	}
	if refreshed.mayPublish() {
		if err := service.submitRefreshedVerdict(ctx, job, summary); err != nil {
			return err
		}
	} else if refreshed.blockWithdrawn {
		logger.InfoContext(
			ctx,
			"verdict refresh withheld",
			slog.String("reason", "block_withdrawn_by_hand"),
			slog.String("decision", string(refreshed.decision)),
		)
	}
	if err := service.upsertSummaryCommentFrom(ctx, job, func(
		state marker.State,
		existingBody string,
	) summaryCommentContent {
		if state.Status == marker.StateReviewing {
			return summaryCommentContent{
				Prose: stripDurableStateMarker(existingBody),
				State: state,
			}
		}
		if state.Status == marker.StateFailed && completedHeadState(state, job.Head) {
			state.Status = marker.StateDone
		}
		return summaryCommentContent{
			Prose: refreshVerdictProse(stripReassessmentNotice(existingBody), summary, refreshed.blockWithdrawn),
			State: state,
		}
	}); err != nil {
		logger.ErrorContext(ctx, "update summary after verdict refresh", slog.String("err", err.Error()))
		return fmt.Errorf("update summary after verdict refresh: %w", err)
	}
	disposition := domain.AssessmentCompleted
	if summary.Decision == domain.ReviewDecisionComment {
		disposition = domain.AssessmentDeclined
	}
	recordAssessment(ctx, disposition, &summary, nil, 0)
	return nil
}

// submitRefreshedVerdict publishes a fresh verdict review at the reviewed head.
func (service *Service) submitRefreshedVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	summary Summary,
) error {
	logger := gklog.L(ctx)
	submitted, err := service.submitInlineBackedVerdict(ctx, job, summary)
	if err != nil {
		logger.ErrorContext(ctx, "submit refreshed verdict", slog.String("err", err.Error()))
		return fmt.Errorf("submit refreshed verdict: %w", err)
	}
	logger.InfoContext(
		ctx,
		"review verdict refreshed",
		slog.Int64("review_id", submitted.ID),
		slog.String("event", string(summary.Decision)),
		slog.Any("blocking", summary.Blocking),
	)
	return nil
}

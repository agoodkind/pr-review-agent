package review

// Admission decides whether a delta is worth attempting at all. An oversized
// review is slow and shallow, so past the configured budget the service says
// it is skipping and why, rather than grinding to a timeout.
//
// Declining is not permission to merge. The check therefore stops short of any
// conclusion GitHub counts as passing, so an oversized and entirely unreviewed
// delta cannot merge on the strength of having been declined.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// compareFileCap is the most files GitHub's compare endpoint will name in one
// range. Past it the response is silently short, so a delta that reaches the
// cap cannot be seen whole and must not be reviewed as though it had been.
const compareFileCap = 300

// AdmissionVerdict reports the ordinary review count and its budget decision.
type AdmissionVerdict struct {
	Skip             bool
	Reason           string
	ReviewableChunks int
}

// CheckReviewBudget checks prepared content after classification and request sizing.
// Relationship assessments use their separate work limit.
func CheckReviewBudget(fileCount int, chunks []diff.Chunk, maxFiles, maxChunks int) AdmissionVerdict {
	reviewable := 0
	for _, chunk := range chunks {
		if chunk.RelationshipReview {
			continue
		}
		for _, piece := range chunk.Pieces {
			if !piece.Oversized && (piece.Metadata != nil || piece.Patch != "") {
				reviewable++
				break
			}
		}
	}
	return admitDelta(fileCount, reviewable, maxFiles, maxChunks)
}

// admitDelta measures the delta against the configured budgets.
//
// The file budget can never exceed what one compare can name. Past that cap the
// range comes back short with nothing saying so, and reviewing it would report
// complete coverage over files nobody listed, which is the one thing this
// service must never do.
func admitDelta(fileCount int, chunkCount int, maxFiles int, maxChunks int) AdmissionVerdict {
	if maxFiles > compareFileCap {
		maxFiles = compareFileCap
	}
	if fileCount > maxFiles {
		return AdmissionVerdict{
			Skip: true, ReviewableChunks: chunkCount,
			Reason: fmt.Sprintf("the changed file count (%d) exceeds the %d file review budget", fileCount, maxFiles),
		}
	}
	if chunkCount > maxChunks {
		return AdmissionVerdict{
			Skip: true, ReviewableChunks: chunkCount,
			Reason: fmt.Sprintf("the reviewable chunk count (%d) exceeds the %d chunk review budget", chunkCount, maxChunks),
		}
	}
	return AdmissionVerdict{Skip: false, Reason: "", ReviewableChunks: chunkCount}
}

// deltaWork records the files and assessment requests for one run.
type deltaWork struct {
	AssessmentRequests int
	FileAssessments    []domain.FileAssessment
	AssessmentModels   []string
	PullRequest        githubapp.PullRequest
	Files              []diff.FileContext
	Metadata           []diff.MetadataSource
	Chunks             []diff.Chunk
}

// collectAndAdmit classifies the current change and prepares bounded review requests.
// An empty base asks the collector for the pull
// request exactly as GitHub presents it. stop reports whether the caller must
// return err immediately: true with a nil err is a completed skip, true with a
// non-nil err is a completed failure, and false means the work is ready for the
// chunk loop.
func (service *Service) collectAndAdmit(
	ctx context.Context,
	job domain.ReviewJob,
	pullRequest githubapp.PullRequest,
	checkRun githubapp.CheckRun,
	base domain.HeadSHA,
	progress *reviewProgress,
	settings reviewSettings,
) (deltaWork, bool, error) {
	logger := gklog.L(ctx)
	var empty deltaWork
	input, err := service.collector.CollectRange(ctx, job.PullRequestRef, pullRequest, base)
	if err != nil {
		logger.ErrorContext(ctx, "collect pull request diff", slog.String("err", err.Error()))
		return empty, true, service.failCheck(
			ctx, job, checkRun.ID, progress.summary(service.now()),
			checkFailureDiff, fmt.Errorf("collect pull request diff: %w", err),
		)
	}
	progress.reached("the diff")

	fileAdmission, deterministic, unresolvedFiles, err := service.admitFileInput(input, settings)
	if err != nil {
		return empty, true, service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureAnalysis, err)
	}
	if fileAdmission.Skip {
		if err := service.declineReview(ctx, job, checkRun.ID, fileAdmission); err != nil {
			return empty, true, service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureSkip, err)
		}
		return empty, true, nil
	}
	contentInput, assessments, assessmentModels, assessmentRequests, err := service.classifyInput(ctx, input, deterministic, unresolvedFiles, settings)
	if err != nil {
		return empty, true, service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureAnalysis, err)
	}
	shortfall := classifyStructuralShortfall(deltaWork{
		AssessmentRequests: assessmentRequests,
		FileAssessments:    assessments,
		AssessmentModels:   assessmentModels,
		PullRequest:        input.PullRequest,
		Files:              input.Files,
		Metadata:           input.Metadata,
		Chunks:             nil,
	})
	promptContext, err := BuildPullRequestContext(input.PullRequest, input.Files, service.reviewPolicy)
	if err != nil {
		return empty, true, service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureAnalysis, err)
	}
	required, err := sourceScopePrompt(shortfall, service.reviewPolicy)
	if err != nil {
		return empty, true, service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureAnalysis, err)
	}
	chunks, err := diff.ChunkInput(contentInput, settings.maximumPromptBytes)
	if err != nil {
		logger.ErrorContext(ctx, "chunk input", slog.String("err", err.Error()))
		return empty, true, service.failCheck(
			ctx, job, checkRun.ID, progress.summary(service.now()),
			checkFailureDiff, fmt.Errorf("chunk input: %w", err),
		)
	}
	work := deltaWork{AssessmentRequests: assessmentRequests, FileAssessments: assessments, AssessmentModels: assessmentModels, PullRequest: input.PullRequest, Files: input.Files, Metadata: input.Metadata, Chunks: chunks}
	work.Chunks, err = BoundModelReviewChunksWithContext(service.model, work.Chunks, work.Files, settings.minimumImportance, required, promptContext, service.reviewPolicy, settings.maximumPromptBytes)
	if err != nil {
		return empty, true, service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureAnalysis, err)
	}
	shortfall = classifyStructuralShortfall(work)
	if shortfall.present() {
		required, err = sourceScopePrompt(shortfall, service.reviewPolicy)
		if err != nil {
			return empty, true, service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureAnalysis, err)
		}
		work.Chunks, err = BoundModelReviewChunksWithContext(service.model, work.Chunks, work.Files, settings.minimumImportance, required, promptContext, service.reviewPolicy, settings.maximumPromptBytes)
		if err != nil {
			return empty, true, service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureAnalysis, err)
		}
	}

	work.Chunks, err = PrepareRelationshipChunks(service.model, work.Chunks, work.Files, settings.minimumImportance, required, promptContext, service.reviewPolicy, settings.maximumPromptBytes)
	if err != nil {
		return empty, true, service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureAnalysis, err)
	}
	chunks = work.Chunks
	verdict := CheckReviewBudget(len(contentInput.Files), chunks, settings.maxFiles, settings.maxChunks)
	if !verdict.Skip {
		return work, false, nil
	}
	logger.InfoContext(
		ctx,
		"review job skipped",
		slog.String("reason", verdict.Reason),
		slog.Int("files", len(input.Files)),
		slog.Int("chunks", len(chunks)),
		slog.Int("reviewable_chunks", verdict.ReviewableChunks),
	)
	if err := service.declineReview(ctx, job, checkRun.ID, verdict); err != nil {
		// A skip that cannot be written is still a run that has to end. Without
		// this the error travels back to the dispatcher, which only logs it, and
		// the check stays in progress with nothing on the pull request saying why.
		return empty, true, service.failCheck(
			ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureSkip, err,
		)
	}
	// The caller stops on a skip and never reads the work, so what was measured
	// travels back unchanged rather than as a second zero value.
	return work, true, nil
}

func (service *Service) admitFileInput(input diff.ReviewInput, settings reviewSettings) (AdmissionVerdict, []domain.FileAssessment, []diff.FileContext, error) {
	if len(input.Files) > compareFileCap {
		return admitDelta(len(input.Files), 0, compareFileCap, settings.maxChunks), nil, nil, nil
	}
	var empty AdmissionVerdict
	deterministic, unresolved, err := PlanFileAssessments(input, service.reviewPolicy)
	if err != nil {
		return empty, nil, nil, err
	}
	reviewableFiles := len(input.Files)
	for _, assessment := range deterministic {
		if assessment.PermitsContentExclusion() && service.reviewPolicy.AllowsContentExclusion(assessment.ContentKind) {
			reviewableFiles--
		}
	}
	return admitDelta(reviewableFiles, 0, settings.maxFiles, settings.maxChunks), deterministic, unresolved, nil
}

// declineReview leaves a delta the admission gate refused exactly as it found
// it. The summary comment says why, the check completes without passing, and no
// review object is touched, because a delta the service cannot review well is
// not a review failure and must never stand as one.
//
// The conclusion is action_required rather than skipped. GitHub counts a
// required check concluded skipped as passing, so concluding that way would let
// an entirely unreviewed oversized delta merge with any earlier approval still
// standing, which is the opposite of what admission is for. action_required
// holds the gate and states the truth of the situation: a person has to split
// the pull request, raise its budget, or ask for the review. The title and the
// summary still say the review was skipped, so a reader can tell "too large to
// review" from "the review broke".
func (service *Service) declineReview(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	verdict AdmissionVerdict,
) error {
	logger := gklog.L(ctx)
	// The state is read and written in one call, so nothing this run keeps has
	// to be carried in from an earlier read that a concurrent edit could have
	// overtaken. It is the same reason the failure notice is written this way.
	if err := service.upsertSummaryCommentFrom(ctx, job, func(existing marker.State, existingBody string) summaryCommentContent {
		prose := RenderSkipBody(verdict.Reason)
		if preserveCheckpointSummary(existing, existingBody) {
			prose = renderCheckpointNotice(existingBody, strings.TrimPrefix(prose, "## Review\n\n"))
		}
		return summaryCommentContent{
			Prose: prose,
			State: marker.State{
				PolicyRevision: existing.PolicyRevision,
				// The baseline stays where it was. Advancing it to a head nobody
				// read would let the next small push measure only its own
				// commits, approve, and merge the declined range as though it
				// had been reviewed, and would let a plain redelivery of the
				// same commit return "already reviewed" with no push at all.
				// The oversized range therefore stays in every later delta and
				// keeps being declined until a person splits the pull request,
				// raises its budget, or asks for the review.
				LastReviewed: existing.LastReviewed,
				RunID:        job.DeliveryID,
				Status:       marker.StateSkipped,
				// A declined delta read no chunk, so it has nothing to add to
				// the work already recorded and no business taking any away.
				// Erasing the chunks an earlier run finished would send the
				// next run that is allowed to review over work this pull
				// request has already paid for.
				Pending:   existing.Pending,
				Completed: existing.Completed,
				// The forcing delivery carries forward for the same reason. A
				// decline is not that delivery finishing, so dropping its record
				// here would send its own resume over the whole pull request
				// again.
				ForcedBy: existing.ForcedBy,
				// So does the recorded shortfall. A decline read nothing, so it
				// cannot have cleared a hunk an earlier run could not read.
				Unread: existing.Unread,
			},
		}
	}); err != nil {
		logger.ErrorContext(ctx, "upsert skip notice", slog.String("err", err.Error()))
		return fmt.Errorf("upsert skip notice: %w", err)
	}
	if err := service.completeCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		checkRunID,
		checkConclusionDeclined,
		checkSummarySkipped,
		verdict.Reason,
	); err != nil {
		logger.ErrorContext(ctx, "complete skipped check run", slog.String("err", err.Error()))
		return fmt.Errorf("complete skipped check run: %w", err)
	}
	logger.InfoContext(ctx, "review job skip completed", slog.Int64("check_run_id", checkRunID))
	return nil
}

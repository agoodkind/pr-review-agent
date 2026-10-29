package review

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// checkpoint saves a completed chunk after GitHub accepts its findings.
// A lock serializes updates to the shared summary comment.
func (service *Service) checkpoint(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	id string,
	tracker *pendingTracker,
	pass *chunkPass,
) error {
	logger := gklog.L(ctx)
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	tracker.unfinished = removeChunkID(tracker.unfinished, id)
	tracker.completed = append(tracker.completed, id)
	tracker.unread = removeChunkID(tracker.unread, id)
	tracker.state.Pending = append([]string{}, tracker.unfinished...)
	tracker.state.Completed = append([]string{}, tracker.completed...)
	tracker.state.Unread = append([]string{}, tracker.unread...)
	tracker.state.RunID = job.DeliveryID
	tracker.state.Status = marker.StateReviewing
	summary := service.progressSummary(ctx, job, pass, tracker.completed)
	ctx, cancel := service.publicationContext(ctx)
	defer cancel()
	err := service.upsertSummaryComment(ctx, job, summaryCommentContent{
		Prose: RenderProgressBody(head, len(tracker.unfinished)) + "\n\n" +
			RenderDetails(summary) + "\n\n" +
			renderMetadataFindings(pass.metadataFindings()) + "\n" +
			encodeFileAssessments(pass.fileAssessments(), service.reviewPolicy.FileAssessmentMaximumBytes()) + "\n" + encodeMetadataRecord(metadataReviewRecord{
			Head: head, Revision: diff.PullRequestRevision(pass.work.PullRequest), Findings: pass.metadataFindings(),
			Omissions: metadataOmissions(pass.structuralShortfall().Hunks), OmissionsAccepted: pass.acceptsOmissions(), DecisionReason: pass.decisionReason(),
		}),
		State: tracker.state,
	})
	if err != nil {
		logger.ErrorContext(ctx, "advance checkpoint", slog.String("err", err.Error()))
		return fmt.Errorf("advance checkpoint: %w", err)
	}
	logger.InfoContext(ctx, "review checkpoint advanced", slog.Int("pending", len(tracker.unfinished)))
	return nil
}

func (service *Service) progressSummary(ctx context.Context, job domain.ReviewJob, pass *chunkPass, completed []string) Summary {
	pass.mu.Lock()
	var summary Summary
	summary.InProgress = true
	summary.Head = job.Head
	summary.Forced = job.Forced
	summary.Duration = service.now().Sub(pass.startedAt)
	summary.MinimumImportance = pass.settings.minimumImportance
	summary.Models = slices.Clone(pass.work.AssessmentModels)
	for _, model := range pass.models.names {
		if !slices.Contains(summary.Models, model) {
			summary.Models = append(summary.Models, model)
		}
	}
	summary.Observed = slices.Clone(pass.collector.observed)
	summary.Eligible = slices.Clone(pass.collector.anchored)
	summary.Published = slices.Clone(pass.published)
	summary.Chunks = len(completed)
	pass.mu.Unlock()
	for _, file := range pass.work.Files {
		if completedFileReview(file, pass.work.Chunks, completed) {
			summary.FilesReviewed++
		}
	}
	summary.Usage = UsageFromContext(ctx)
	summary.FileAssessments = pass.fileAssessments()
	summary.FileAssessmentsDisplay = service.reviewPolicy.FileAssessmentsDisplay()
	summary.FileAssessmentMaximumBytes = service.reviewPolicy.FileAssessmentMaximumBytes()
	service.enrichProviderPeriodUsage(ctx, &summary)
	return summary
}

func completedFileReview(file diff.FileContext, chunks []diff.Chunk, completed []string) bool {
	if !file.CoverageComplete {
		return false
	}
	found := false
	for _, chunk := range chunks {
		if chunk.RelationshipReview || !slices.Contains(chunk.Paths, file.Path) {
			continue
		}
		found = true
		if !chunk.CoverageComplete || !slices.Contains(completed, chunkID(chunk)) {
			return false
		}
	}
	return found
}

// reviewProgress records what one review has learned so far. A review that
// stops early reports this, so the reader sees which model answered, how much
// it read, and where it stopped, rather than a bare error with no context.
type reviewProgress struct {
	fileAssessmentMaximumBytes int
	fileAssessments            []domain.FileAssessment
	fileAssessmentsDisplay     reviewrules.FindingDisplay
	head                       domain.HeadSHA
	startedAt                  time.Time
	minimumImportance          int
	stage                      string
	models                     []string
	filesReviewed              int
	chunks                     int
	observed                   []domain.Finding
	eligible                   []domain.Finding
	published                  []domain.Finding
	metadata                   []domain.Finding
	metadataRevision           string
	metadataFindingsDisplay    reviewrules.FindingDisplay
	priorReviews               []reviewTrace
	threads                    []threadTrace
	usage                      *UsageRecorder
	// forced records that a label asked for this run, so every summary rendered
	// from this progress reports the same trigger the successful one does.
	forced bool
}

func newReviewProgress(
	head domain.HeadSHA,
	startedAt time.Time,
	minimumImportance int,
	forced bool,
	metadataFindingsDisplay reviewrules.FindingDisplay,
) *reviewProgress {
	return &reviewProgress{
		fileAssessmentMaximumBytes: 0,
		fileAssessments:            nil,
		fileAssessmentsDisplay:     reviewrules.FindingsCollapsed,
		head:                       head,
		startedAt:                  startedAt,
		minimumImportance:          minimumImportance,
		stage:                      "",
		models:                     nil,
		filesReviewed:              0,
		chunks:                     0,
		observed:                   nil,
		eligible:                   nil,
		published:                  nil,
		metadata:                   nil,
		metadataRevision:           "",
		metadataFindingsDisplay:    metadataFindingsDisplay,
		priorReviews:               nil,
		threads:                    nil,
		usage:                      nil,
		forced:                     forced,
	}
}

// applyPublished records the findings that already reached the pull request, so
// a failed run reports the comments a reader can see rather than none.
func (progress *reviewProgress) applyPublished(published []domain.Finding) {
	progress.published = published
}

// reached names the last stage the review finished.
func (progress *reviewProgress) reached(stage string) {
	progress.stage = stage
}

// applyAnalysis records what the model analysis learned, including a partial
// result from an analysis that failed part way through.
func (progress *reviewProgress) applyAnalysis(analysis Analysis) {
	progress.models = analysis.Models
	progress.filesReviewed = analysis.FilesReviewed
	progress.chunks = analysis.Chunks
	progress.observed = analysis.Observed
	progress.eligible = analysis.Anchored
}

// summary renders the progress as the value both failure outputs report from.
func (progress *reviewProgress) summary(now time.Time) Summary {
	return Summary{
		FileAssessmentNote: "",
		Head:               progress.head,
		Decision:           "",
		DecisionReason:     "",
		ApprovalWithheld:   true,
		Report: Report{
			Summary:     "",
			Walkthrough: nil,
		},
		OmissionsAccepted: false,
		// A failed run carries no verdict, so there is nothing for it to be
		// waiting on and nothing to name.
		Blocking:          nil,
		Models:            progress.models,
		Usage:             progress.usage.Summary(),
		Duration:          now.Sub(progress.startedAt),
		FilesReviewed:     progress.filesReviewed,
		Chunks:            progress.chunks,
		CoverageComplete:  false,
		MinimumImportance: progress.minimumImportance,
		Observed:          progress.observed,
		Eligible:          progress.eligible,
		// Findings post while the review runs, so a failed run can still leave
		// comments on the page. Reporting none here would contradict what the
		// reader sees on the same pull request.
		Published:                  progress.published,
		Fallback:                   nil,
		Metadata:                   progress.metadata,
		MetadataRevision:           progress.metadataRevision,
		MetadataFindingsDisplay:    progress.metadataFindingsDisplay,
		Omissions:                  nil,
		PriorReviews:               progress.priorReviews,
		Threads:                    progress.threads,
		Reached:                    progress.stage,
		Failed:                     true,
		InProgress:                 false,
		Forced:                     progress.forced,
		FileAssessments:            progress.fileAssessments,
		FileAssessmentsDisplay:     progress.fileAssessmentsDisplay,
		FileAssessmentMaximumBytes: progress.fileAssessmentMaximumBytes,
	}
}

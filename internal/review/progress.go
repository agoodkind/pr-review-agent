package review

import (
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
)

// reviewProgress records what one review has learned so far. A review that
// stops early reports this, so the reader sees which model answered, how much
// it read, and where it stopped, rather than a bare error with no context.
type reviewProgress struct {
	head              domain.HeadSHA
	startedAt         time.Time
	minimumImportance int
	stage             string
	models            []string
	filesReviewed     int
	chunks            int
	observed          []domain.Finding
	eligible          []domain.Finding
	priorReviews      []reviewTrace
	threads           []threadTrace
}

func newReviewProgress(head domain.HeadSHA, startedAt time.Time, minimumImportance int) *reviewProgress {
	return &reviewProgress{
		head:              head,
		startedAt:         startedAt,
		minimumImportance: minimumImportance,
		stage:             "",
		models:            nil,
		filesReviewed:     0,
		chunks:            0,
		observed:          nil,
		eligible:          nil,
		priorReviews:      nil,
		threads:           nil,
	}
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
		Head:              progress.head,
		Decision:          "",
		Models:            progress.models,
		Duration:          now.Sub(progress.startedAt),
		FilesReviewed:     progress.filesReviewed,
		Chunks:            progress.chunks,
		CoverageComplete:  false,
		MinimumImportance: progress.minimumImportance,
		Observed:          progress.observed,
		Eligible:          progress.eligible,
		Published:         nil,
		PriorReviews:      progress.priorReviews,
		Threads:           progress.threads,
		Reached:           progress.stage,
		Failed:            true,
	}
}

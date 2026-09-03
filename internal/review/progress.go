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
	published         []domain.Finding
	priorReviews      []reviewTrace
	threads           []threadTrace
	// forced records that a label asked for this run, so every summary rendered
	// from this progress reports the same trigger the successful one does.
	forced bool
}

func newReviewProgress(
	head domain.HeadSHA,
	startedAt time.Time,
	minimumImportance int,
	forced bool,
) *reviewProgress {
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
		published:         nil,
		priorReviews:      nil,
		threads:           nil,
		forced:            forced,
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
		Head:     progress.head,
		Decision: "",
		// A failed run carries no verdict, so there is nothing for it to be
		// waiting on and nothing to name.
		Blocking:          nil,
		Models:            progress.models,
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
		Published:    progress.published,
		PriorReviews: progress.priorReviews,
		Threads:      progress.threads,
		Reached:      progress.stage,
		Failed:       true,
		Forced:       progress.forced,
	}
}

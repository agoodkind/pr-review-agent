package review

// This file publishes findings while the review is still running.
//
// A review that holds everything until the last chunk answers gives the reader
// nothing when it stops early, and the run stops early often: a provider
// refusal, a dropped connection, an expired deadline, or the container being
// reclaimed mid review. Posting each finding as its chunk produces it means the
// work already done reaches the pull request no matter what happens to the rest.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
)

// FindingSink receives findings as the review produces them.
type FindingSink interface {
	// Publish posts the findings from one chunk that are worth publishing.
	Publish(ctx context.Context, findings []domain.Finding)
}

// discardSink accepts findings and publishes nothing. Analysis uses it when no
// destination is wired, which keeps analysis usable on its own.
type discardSink struct{}

// Publish accepts the findings and posts none of them.
func (discardSink) Publish(context.Context, []domain.Finding) {}

// streamingSink posts each finding as its own review comment.
//
// It holds the selection state publication needs, so the same suppression and
// capacity rules apply whether a finding is posted mid review or at the end.
// Chunks run concurrently, so every read and write of that state is guarded.
type streamingSink struct {
	github    GitHub
	job       domain.ReviewJob
	head      domain.HeadSHA
	selection *publicationState
	// postTimeout bounds one batch of comment posts. Each batch gets its own,
	// because batches arrive spread across the whole analysis.
	postTimeout time.Duration
	mu          sync.Mutex
	admitted    []domain.Finding
	posted      int
	failed      int
}

// newStreamingSink builds a sink over the publication state a run starts with.
func newStreamingSink(
	github GitHub,
	job domain.ReviewJob,
	head domain.HeadSHA,
	selection *publicationState,
	postTimeout time.Duration,
) *streamingSink {
	return &streamingSink{
		github:      github,
		job:         job,
		head:        head,
		selection:   selection,
		postTimeout: postTimeout,
		mu:          sync.Mutex{},
		admitted:    make([]domain.Finding, 0),
		posted:      0,
		failed:      0,
	}
}

// Publish posts every finding from one chunk that the selection rules admit.
//
// A comment GitHub rejects is counted and left alone rather than retried. The
// review still stands behind the finding, because the defect is real whether or
// not its comment reached the page, and the next push offers it again.
func (sink *streamingSink) Publish(ctx context.Context, findings []domain.Finding) {
	logger := gklog.L(ctx)
	admitted := sink.admit(findings)
	if len(admitted) == 0 {
		return
	}

	comments, err := RenderInline(sink.head, admitted)
	if err != nil {
		logger.ErrorContext(ctx, "render streamed findings", slog.String("err", err.Error()))
		return
	}

	// Posting runs free of the analysis deadline. The last chunk to answer
	// answers near that deadline, so posting on the analysis context would fail
	// exactly the findings this whole mechanism exists to deliver, and the
	// review would then claim comments the reader cannot see.
	ctx, cancel := detachFromReviewDeadline(ctx, sink.postTimeout)
	defer cancel()

	// A push during analysis supersedes the commit these findings describe.
	// Posting them anyway would leave comments on a commit nobody is looking at,
	// and the reader would see objections to code they have already replaced.
	if !sink.headIsCurrent(ctx) {
		logger.InfoContext(
			ctx,
			"streamed findings dropped",
			slog.String("reason", "head moved during analysis"),
			slog.Int("findings", len(admitted)),
		)
		return
	}

	posted := 0
	rejected := 0
	for _, comment := range comments {
		if postErr := sink.github.CreateReviewComment(
			ctx,
			sink.job.InstallationID,
			sink.job.Repository,
			sink.job.Number,
			sink.head,
			comment,
		); postErr != nil {
			logger.ErrorContext(
				ctx,
				"publish streamed finding",
				slog.String("path", comment.Path),
				slog.Int("line", comment.Line),
				slog.String("err", postErr.Error()),
			)
			rejected++
			continue
		}
		posted++
	}
	sink.count(posted, rejected)
	logger.InfoContext(
		ctx,
		"review findings streamed",
		slog.Int("offered", len(findings)),
		slog.Int("admitted", len(admitted)),
		slog.Int("posted", posted),
		slog.Int("rejected", rejected),
	)
}

// headIsCurrent reports whether the pull request still points at the commit
// these findings describe. A read that fails is treated as current, because
// dropping real findings over one failed lookup costs the reader more than a
// comment on a commit that just moved.
func (sink *streamingSink) headIsCurrent(ctx context.Context) bool {
	pullRequest, err := sink.github.GetPullRequest(
		ctx,
		sink.job.InstallationID,
		sink.job.Repository,
		sink.job.Number,
	)
	if err != nil {
		gklog.L(ctx).ErrorContext(
			ctx,
			"read head before streaming findings",
			slog.String("err", err.Error()),
		)
		return true
	}
	return pullRequest.Head == sink.head
}

// admit reserves capacity for the findings this chunk may publish, so two
// chunks finishing together cannot both spend the last slot.
func (sink *streamingSink) admit(findings []domain.Finding) []domain.Finding {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	admitted := sink.selection.admit(findings)
	sink.admitted = append(sink.admitted, admitted...)
	return admitted
}

// count records one chunk's delivery result in a single update, so posting a
// batch takes the lock once rather than once per comment.
func (sink *streamingSink) count(posted int, rejected int) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.posted += posted
	sink.failed += rejected
}

// Objections returns every finding this review stands behind, whether or not
// its comment reached the page. A defect is real regardless of what GitHub did
// with the comment describing it, so approving over a rejected comment would
// ship the defect.
func (sink *streamingSink) Objections() []domain.Finding {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]domain.Finding{}, sink.admitted...)
}

// Delivery reports how many comments reached the pull request and how many
// GitHub rejected, so the run says whether the reader can see what was found.
func (sink *streamingSink) Delivery() (int, int) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.posted, sink.failed
}

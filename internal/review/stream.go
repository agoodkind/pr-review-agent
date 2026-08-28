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
	"goodkind.io/pr-review-agent/internal/githubapp"
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
//
// One reservation, the tail slot, is never posted the moment it is admitted.
// It is held so a later chunk's more important finding can still take it,
// resolved only at Finalize once every chunk has answered.
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
	finalist    *heldFinalist
}

// heldFinalist is the run's one reservation not yet posted, and where its
// finding lives in admitted, so a later winner can replace it without a search.
type heldFinalist struct {
	item          candidate
	admittedIndex int
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
		finalist:    nil,
	}
}

// Publish posts every finding from one chunk that the selection rules admit
// immediately. One reservation may instead become the held tail slot, in which
// case it posts later, at Finalize.
func (sink *streamingSink) Publish(ctx context.Context, findings []domain.Finding) {
	toPostNow := sink.considerBatch(findings)
	if len(toPostNow) == 0 {
		return
	}
	sink.deliver(ctx, toPostNow)
}

// Finalize posts the finding holding the run's tail slot, once every chunk has
// answered and no later arrival can still outrank it.
func (sink *streamingSink) Finalize(ctx context.Context) {
	sink.mu.Lock()
	var pending *candidate
	if sink.finalist != nil {
		item := sink.finalist.item
		pending = &item
	}
	sink.mu.Unlock()
	if pending == nil {
		return
	}
	sink.deliver(ctx, []candidate{*pending})
}

// considerBatch decides which findings from one chunk post immediately, and
// updates which finding, if any, holds the run's tail slot.
//
// Every slot but one is spent on a first-come basis, same as before this fix.
// The last slot is never spent immediately. It is held for whichever finding
// turns out most important, decided only once every chunk has answered, so a
// low importance finding that happens to answer first cannot take the slot a
// more severe finding, arriving later, would have won.
func (sink *streamingSink) considerBatch(findings []domain.Finding) []candidate {
	sink.mu.Lock()
	defer sink.mu.Unlock()

	candidates := make([]candidate, 0, len(findings))
	for _, finding := range findings {
		keys := keysFor(finding)
		if sink.selection.suppressed(keys) {
			continue
		}
		candidates = append(candidates, candidate{finding: finding, keys: keys})
	}
	sortByImportanceThenPosition(candidates)

	toPostNow := make([]candidate, 0, len(candidates))
	for _, item := range candidates {
		if sink.selection.capacity > 0 {
			sink.selection.capacity--
			sink.admitted = append(sink.admitted, item.finding)
			toPostNow = append(toPostNow, item)
			continue
		}
		if !sink.selection.hasTailSlot {
			continue
		}
		if sink.finalist == nil {
			sink.admitted = append(sink.admitted, item.finding)
			sink.finalist = &heldFinalist{item: item, admittedIndex: len(sink.admitted) - 1}
			continue
		}
		if !outranks(item, sink.finalist.item) {
			continue
		}
		sink.admitted[sink.finalist.admittedIndex] = item.finding
		sink.finalist.item = item
	}
	return toPostNow
}

// postCandidate pairs one reservation with its rendered comment, so posting
// never has to line two separately-ordered slices back up by index.
type postCandidate struct {
	item    candidate
	comment githubapp.InlineComment
}

// deliver renders and posts one batch of reservations, then settles each one:
// confirmed once its comment posts, released if it does not. A comment GitHub
// rejects is counted and left alone rather than retried. The review still
// stands behind the finding, because the defect is real whether or not its
// comment reached the page, and the next push offers it again.
func (sink *streamingSink) deliver(ctx context.Context, items []candidate) {
	logger := gklog.L(ctx)
	// RenderInline sorts its whole input by path and line, so a batched call
	// would return comments in an order that no longer lines up with items.
	// Rendering one finding at a time and carrying the comment alongside its
	// candidate avoids depending on two slices staying in lockstep.
	posts := make([]postCandidate, 0, len(items))
	for _, item := range items {
		rendered, err := RenderInline(sink.head, []domain.Finding{item.finding})
		if err != nil {
			logger.ErrorContext(
				ctx,
				"render streamed finding",
				slog.String("path", item.finding.Path),
				slog.String("err", err.Error()),
			)
			sink.settle(nil, items)
			return
		}
		posts = append(posts, postCandidate{item: item, comment: rendered[0]})
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
	// The run's own head check cancels the whole check afterward, so nothing
	// here needs to release these reservations.
	if !sink.headIsCurrent(ctx) {
		logger.InfoContext(
			ctx,
			"streamed findings dropped",
			slog.String("reason", "head moved during analysis"),
			slog.Int("findings", len(items)),
		)
		return
	}

	delivered := make([]candidate, 0, len(items))
	undelivered := make([]candidate, 0)
	for _, post := range posts {
		if postErr := sink.github.CreateReviewComment(
			ctx,
			sink.job.InstallationID,
			sink.job.Repository,
			sink.job.Number,
			sink.head,
			post.comment,
		); postErr != nil {
			logger.ErrorContext(
				ctx,
				"publish streamed finding",
				slog.String("path", post.comment.Path),
				slog.Int("line", post.comment.Line),
				slog.String("err", postErr.Error()),
			)
			undelivered = append(undelivered, post.item)
			continue
		}
		delivered = append(delivered, post.item)
	}
	sink.settle(delivered, undelivered)
	logger.InfoContext(
		ctx,
		"review findings streamed",
		slog.Int("offered", len(items)),
		slog.Int("posted", len(delivered)),
		slog.Int("rejected", len(undelivered)),
	)
}

// settle records one batch's delivery outcome under a single lock.
//
// A delivered finding is remembered so no later chunk repeats it and no future
// run reports it again. An undelivered finding releases the slot it reserved,
// because it was never shown to anyone: a rejected comment must not cost a
// different, later finding its only chance at that slot, and must not be
// suppressed from ever being reported again.
func (sink *streamingSink) settle(delivered []candidate, undelivered []candidate) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, item := range delivered {
		sink.selection.remember(item.keys)
		if sink.finalist != nil && sink.finalist.item.keys == item.keys {
			sink.finalist = nil
		}
	}
	for _, item := range undelivered {
		if sink.finalist != nil && sink.finalist.item.keys == item.keys {
			sink.finalist = nil
			continue
		}
		sink.selection.capacity++
	}
	sink.posted += len(delivered)
	sink.failed += len(undelivered)
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

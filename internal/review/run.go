package review

// This file reviews the delta chunk by chunk and checkpoints after each one.
//
// A review that holds its progress in process memory loses everything when the
// container dies, and a review under one shared clock never finishes a large
// diff: 31 logged timeouts were that one clock colliding with unbounded input.
// Here every chunk gets its own model call under its own timeout, posts what it
// found, and only then advances the durable checkpoint. A death at any moment
// loses at most the chunks in flight, and a chunk whose call or post failed
// stays pending and visible for the next push.
//
// Chunks run several at a time. Concurrency is not a clock, so it takes nothing
// away from the rule that no clock spans two model calls; what it buys is wall
// clock, because a sixty chunk delta reviewed strictly one at a time would run
// for hours.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// chunkIDLength is how much of a chunk's digest names it in the durable marker.
// Twelve hex characters keep a sixty chunk list short enough to sit in one HTML
// comment while leaving collision odds negligible for one pull request.
const chunkIDLength = 12

// errHeadMoved reports that the pull request moved off the commit this run
// read, so nothing more may be written about that commit.
var errHeadMoved = errors.New("the pull request head moved during the review")

// errCommentRefused reports that GitHub answered and refused a comment.
//
// That is not a transient failure. The chunk was read, and the comment it
// produced can never post, so holding the chunk pending would pin the pull
// request on an attempt already known to fail: every later run would re-derive
// the same chunk, be refused the same way, and never advance the checkpoint.
var errCommentRefused = errors.New("github refused a review comment")

// chunkID names one chunk by its content rather than by its position.
//
// The checkpoint written by one run has to match the same chunk when a later
// run re-derives the delta, and position does not survive that: a new commit
// shifts every index. The digest of the chunk text does survive, so a chunk
// left pending is recognized again exactly when its text is unchanged.
func chunkID(chunk diff.Chunk) string {
	digest := sha256.Sum256([]byte(chunk.Text))
	return hex.EncodeToString(digest[:])[:chunkIDLength]
}

// removeChunkID drops one id from a pending list, keeping the rest in order.
func removeChunkID(pending []string, id string) []string {
	remaining := make([]string, 0, len(pending))
	for _, item := range pending {
		if item == id {
			continue
		}
		remaining = append(remaining, item)
	}
	return remaining
}

// chunkPass accumulates what one pass over the chunks learned, so the run can
// report the same detail table a single shot analysis used to report.
//
// Chunks answer concurrently, so every field here is guarded. The model call
// and the comment posts happen outside the lock; only the bookkeeping is inside.
type chunkPass struct {
	work deltaWork
	// settings are the values this run is bound by, carried here so a chunk
	// reads what the delivery asked for rather than what the process booted
	// with. They are written once and read concurrently without the lock.
	settings  reviewSettings
	selection *publicationState
	// disputes and disputePrompt are what the pull request has already been
	// told, as keys for the backstop and as prose for the prompt. Both are built
	// once and never written again, so the chunks read them concurrently without
	// the lock.
	disputes      disputeContext
	disputePrompt string
	// carried names what the pull request was already waiting on when this pass
	// started, taken from the service's own open threads. A resumed pass posts
	// only the findings it reads itself, so without this the progress comment
	// would drop everything an earlier pass had already put on the page. It is
	// built once and never written again, so the chunks read it without the
	// lock.
	carried []string

	mu        sync.Mutex
	collector *findingCollector
	models    modelSet
	published []domain.Finding
	failures  []chunkFailure
	coverage  bool
	requests  int
	posted    int
	failed    int
	panicked  *chunkPanicError
}

// chunkPanicError marks a chunk that panicked, so the run reports an internal
// panic rather than blaming the model.
//
// A panic inside a chunk cannot travel as a panic. Unrecovered in a goroutine
// it ends the whole process, losing the run and every checkpoint it wrote, so
// the goroutine recovers and the failure travels back as an error.
type chunkPanicError struct {
	chunk int
	value string
}

func (err *chunkPanicError) Error() string {
	return fmt.Sprintf("review chunk %d panicked: %s", err.chunk, err.value)
}

// isChunkPanic reports whether a review failure came from a panic inside a
// chunk rather than from the model or the provider.
func isChunkPanic(err error) bool {
	var target *chunkPanicError
	return errors.As(err, &target)
}

func newChunkPass(
	work deltaWork,
	settings reviewSettings,
	selection *publicationState,
	disputes disputeContext,
	carried []string,
) *chunkPass {
	return &chunkPass{
		work:          work,
		settings:      settings,
		selection:     selection,
		disputes:      disputes,
		disputePrompt: disputes.promptSection(),
		carried:       carried,
		mu:            sync.Mutex{},
		collector:     newFindingCollector(work.Files, settings.minimumImportance),
		models:        modelSet{names: nil, seen: nil},
		published:     make([]domain.Finding, 0),
		failures:      make([]chunkFailure, 0),
		coverage:      inputCoverageComplete(work.Files) && chunksCoverageComplete(work.Chunks),
		requests:      0,
		posted:        0,
		failed:        0,
		panicked:      nil,
	}
}

// chunksCoverageComplete reports whether every chunk carries its whole hunk.
func chunksCoverageComplete(chunks []diff.Chunk) bool {
	for _, chunk := range chunks {
		if !chunk.CoverageComplete {
			return false
		}
	}
	return true
}

// recordCall folds one chunk's own model accounting back into the pass. Each
// call keeps its own so nothing is written concurrently through one pointer.
func (pass *chunkPass) recordCall(models modelSet, requests int, results []domain.ReviewResult) {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	for _, name := range models.names {
		pass.models.add(name)
	}
	pass.requests += requests
	for _, result := range results {
		if !result.CoverageComplete {
			pass.coverage = false
		}
	}
}

// recordConsolidationRequest counts the extra model call a chunk spent asking
// whether its own findings restate each other, so the request count a run
// reports covers every call it made rather than only the chunk calls.
func (pass *chunkPass) recordConsolidationRequest() {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	pass.requests++
}

// recordFailure records one chunk nobody could read. A chunk that went unread
// is a slice of the head nobody covered, so it also ends the coverage claim.
func (pass *chunkPass) recordFailure(chunk int, err error) {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	pass.failures = append(pass.failures, chunkFailure{chunk: chunk, err: err})
	pass.coverage = false
}

// recordPanic keeps the first panic a chunk raised, for the caller to report
// as the run's failure.
func (pass *chunkPass) recordPanic(chunk int, value string) {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	if pass.panicked == nil {
		pass.panicked = &chunkPanicError{chunk: chunk, value: value}
	}
}

// panicked reports the first panic a chunk raised, or nil when none did.
func (pass *chunkPass) chunkPanic() error {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	if pass.panicked == nil {
		return nil
	}
	return pass.panicked
}

// unreadChunks reports the failures in chunk order, so every surface that names
// them reads the same whichever chunk answered first.
func (pass *chunkPass) unreadChunks() []chunkFailure {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	ordered := append([]chunkFailure{}, pass.failures...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].chunk < ordered[right].chunk
	})
	return ordered
}

// analysis renders the pass as the value the summary and the check run render
// from, so a partial pass reports the same shape a complete one does.
func (pass *chunkPass) analysis() Analysis {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	sortFindings(pass.collector.observed)
	sortFindings(pass.collector.anchored)
	return Analysis{
		CoverageComplete: pass.coverage,
		Observed:         pass.collector.observed,
		Anchored:         pass.collector.anchored,
		Decision:         DecisionFor(pass.collector.anchored, pass.collector.minimumImportance),
		FilesReviewed:    len(pass.work.Files),
		Chunks:           len(pass.work.Chunks),
		Models:           pass.models.names,
	}
}

// delivery reports what reached the page and what did not.
func (pass *chunkPass) delivery() (posted int, failed int) {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	return pass.posted, pass.failed
}

// requestCount reports how many model requests the pass spent, the truncation
// split's extra halves included.
func (pass *chunkPass) requestCount() int {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	return pass.requests
}

// publishedFindings returns every finding whose comment reached the page.
func (pass *chunkPass) publishedFindings() []domain.Finding {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	return append([]domain.Finding{}, pass.published...)
}

// reviewDelta reviews the delta's chunks, posting each chunk's findings before
// advancing the checkpoint, so a death at any moment loses at most the chunks
// in flight.
//
// Nothing is retried. A chunk whose call or post failed stays pending and the
// pass moves on, because a retry inside one invocation spends the same budget
// on the same failure while the reader waits.
func (service *Service) reviewDelta(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	state marker.State,
	pass *chunkPass,
) (marker.State, error) {
	logger := gklog.L(ctx)
	work := pendingWork(ctx, state, pass.work.Chunks)
	logger.InfoContext(
		ctx,
		"review model analysis started",
		slog.Int("chunks", len(pass.work.Chunks)),
		slog.Int("pending", len(work.owed)),
		slog.Int("concurrency", min(config.MaximumChunkConcurrency, len(work.owed))),
	)

	tracker := &pendingTracker{
		mu:         sync.Mutex{},
		unfinished: work.owed,
		completed:  work.completed,
		state:      state,
	}
	fatal := service.reviewChunksConcurrently(ctx, job, head, work.chunks, pass, tracker)
	// A panic ends the run rather than leaving a chunk pending: it is a defect
	// here, not a provider having a bad minute, and the next push would hit it
	// again.
	if panicked := pass.chunkPanic(); panicked != nil {
		return tracker.snapshot(), panicked
	}
	if fatal != nil {
		return tracker.snapshot(), fatal
	}
	return concludeState(
		tracker.snapshot(), job, head, tracker, classifyStructuralShortfall(pass.work).present(),
	), nil
}

// deltaOwed is the work one pass has to do: the chunks to review, the ids they
// carry, and the ids already read that carry over into the next checkpoint.
type deltaOwed struct {
	chunks    []diff.Chunk
	owed      []string
	completed []string
}

// pendingWork is the delta as it stands now, minus the chunks already read
// since the last reviewed commit.
//
// Two things have to hold at once. A chunk a new commit introduced must be
// reviewed, which is why the work starts from the whole delta rather than from
// the pending list: the last reviewed commit does not advance while anything is
// pending, so the next delta covers the old range plus everything pushed since,
// and reviewing only the pending ids would skip those commits and then declare
// the whole range reviewed. A chunk an earlier run already read must not be
// paid for twice, which is why the completed set is subtracted rather than the
// delta being re-read whole.
//
// The completed set carries only ids the current delta still holds. A chunk
// whose text has changed can never match again, so keeping its id would grow
// the marker for nothing; pruning here bounds the set by the delta, which
// admission already bounds.
func pendingWork(ctx context.Context, state marker.State, chunks []diff.Chunk) deltaOwed {
	logger := gklog.L(ctx)
	done := make(map[string]struct{}, len(state.Completed))
	for _, id := range state.Completed {
		done[id] = struct{}{}
	}

	owed := make([]string, 0, len(chunks))
	carried := make([]string, 0, len(state.Completed))
	remaining := make([]diff.Chunk, 0, len(chunks))
	present := make(map[string]struct{}, len(chunks))
	for _, chunk := range chunks {
		id := chunkID(chunk)
		present[id] = struct{}{}
		if _, alreadyRead := done[id]; alreadyRead {
			carried = append(carried, id)
			continue
		}
		remaining = append(remaining, chunk)
		owed = append(owed, id)
	}
	for _, id := range state.Pending {
		if _, found := present[id]; !found {
			logger.WarnContext(
				ctx,
				"pending chunk is no longer in the delta",
				slog.String("chunk", id),
			)
		}
	}
	logger.InfoContext(
		ctx,
		"review work computed",
		slog.Int("delta_chunks", len(chunks)),
		slog.Int("already_read", len(carried)),
		slog.Int("owed", len(owed)),
	)
	return deltaOwed{chunks: remaining, owed: owed, completed: carried}
}

// pendingTracker owns the pending list, the completed list, and the durable
// state while chunks answer concurrently. Every checkpoint is a read and a
// write of one comment, so they are serialized here rather than racing.
type pendingTracker struct {
	mu         sync.Mutex
	unfinished []string
	completed  []string
	state      marker.State
}

func (tracker *pendingTracker) snapshot() marker.State {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.state
}

func (tracker *pendingTracker) remaining() []string {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return append([]string{}, tracker.unfinished...)
}

// finished returns the chunk ids read since the last reviewed commit, this
// pass's and the earlier passes' alike.
func (tracker *pendingTracker) finished() []string {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return append([]string{}, tracker.completed...)
}

// reviewChunksConcurrently reviews the owed chunks a bounded number at a time,
// and returns the one failure class that ends the whole run rather than leaving
// a chunk pending.
func (service *Service) reviewChunksConcurrently(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	chunks []diff.Chunk,
	pass *chunkPass,
	tracker *pendingTracker,
) error {
	limit := min(config.MaximumChunkConcurrency, len(chunks))
	if limit < 1 {
		return nil
	}
	slots := make(chan struct{}, limit)
	fatal := make(chan error, len(chunks))

	var waitGroup sync.WaitGroup
	for _, chunk := range chunks {
		waitGroup.Go(func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					gklog.L(ctx).ErrorContext(
						ctx,
						"review chunk panicked",
						slog.Int("chunk", chunk.Index),
						slog.Any("panic", recovered),
						slog.String("err", "review chunk panicked"),
					)
					pass.recordPanic(chunk.Index, fmt.Sprint(recovered))
				}
			}()
			slots <- struct{}{}
			err := service.reviewOneChunk(ctx, job, head, chunk, pass)
			<-slots
			if endsRun := service.settleChunk(ctx, job, head, chunk, err, pass, tracker); endsRun != nil {
				fatal <- endsRun
			}
		})
	}
	waitGroup.Wait()

	close(fatal)
	return <-fatal
}

// settleChunk records one chunk's outcome and reports the failures that end the
// whole run instead of leaving the chunk pending.
func (service *Service) settleChunk(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	chunk diff.Chunk,
	err error,
	pass *chunkPass,
	tracker *pendingTracker,
) error {
	logger := gklog.L(ctx)
	id := chunkID(chunk)
	switch {
	case err == nil:
	case errors.Is(err, errHeadMoved):
		return err
	case ctx.Err() != nil:
		// A stopping service is not one failed chunk.
		return fmt.Errorf("review cancelled: %w", ctx.Err())
	case errors.Is(err, errCommentRefused):
		// GitHub answered and refused. The chunk was read and its comment can
		// never post, so it is finished rather than owed. The run still refuses
		// to approve, because a finding nobody can see is still a finding.
		logger.WarnContext(
			ctx,
			"chunk finished with a comment github refused",
			slog.String("chunk", id),
			slog.String("err", err.Error()),
		)
	default:
		pass.recordFailure(chunk.Index, err)
		logger.ErrorContext(
			ctx,
			"chunk review failed, leaving it pending",
			slog.String("chunk", id),
			slog.String("err", err.Error()),
		)
		return nil
	}
	return service.checkpoint(ctx, job, head, id, tracker, pass)
}

// checkpoint records that one chunk is finished, after its findings are on the
// page and never before. The write is what makes the progress survive a death,
// and it is serialized because it reads and rewrites one comment.
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

	// The findings are read inside the tracker lock, so the order checkpoints
	// take that lock is also the order their snapshots were taken. Reading first
	// let a chunk that acquired the lock late overwrite the comment with a list
	// that predated a newer finding. Nothing holds the pass lock while waiting
	// on this one, so taking it here cannot deadlock.
	published := pass.publishedFindings()
	waiting := mergeLocations(pass.carried, findingLocations(published))

	tracker.unfinished = removeChunkID(tracker.unfinished, id)
	tracker.completed = append(tracker.completed, id)
	tracker.state.Pending = append([]string{}, tracker.unfinished...)
	tracker.state.Completed = append([]string{}, tracker.completed...)
	tracker.state.RunID = job.DeliveryID
	tracker.state.Status = marker.StateReviewing
	err := service.upsertSummaryComment(ctx, job, summaryCommentContent{
		Prose: RenderProgressBody(head, len(tracker.unfinished), waiting),
		State: tracker.state,
	})
	if err != nil {
		logger.ErrorContext(ctx, "advance checkpoint", slog.String("err", err.Error()))
		return fmt.Errorf("advance checkpoint: %w", err)
	}
	logger.InfoContext(ctx, "review checkpoint advanced", slog.Int("pending", len(tracker.unfinished)))
	return nil
}

// concludeState closes the pass out. The last reviewed commit advances only
// when nothing is left pending, so a run that could not read the whole head
// never claims it did.
//
// Advancing it also drops the completed set. That set exists to keep the next
// run from re-reading chunks under the same baseline; once the baseline moves,
// the next delta starts after them and every id in it names a chunk that can
// never appear again.
//
// unreadable says the delta holds something no run can read, such as a hunk
// larger than one model request. Nothing is pending in that case, because every
// chunk answered, so the baseline would otherwise advance over code nobody read
// and the next delta would start after it. Holding it keeps that code in every
// later delta, exactly as a declined delta stays in one. The completed set is
// kept for the same reason it is kept while chunks are pending: the chunks that
// did answer must not be paid for twice.
func concludeState(
	state marker.State,
	job domain.ReviewJob,
	head domain.HeadSHA,
	tracker *pendingTracker,
	unreadable bool,
) marker.State {
	unfinished := tracker.remaining()
	state.Pending = unfinished
	state.Completed = tracker.finished()
	state.RunID = job.DeliveryID
	state.Status = marker.StateReviewing
	if unreadable {
		return state
	}
	if len(unfinished) == 0 {
		state.LastReviewed = head
		state.Status = marker.StateDone
		state.Completed = nil
	}
	return state
}

// reviewOneChunk makes one chunk's model call under its own timeout and posts
// what that call found.
//
// The timeout is built from the caller's context on every call, so no chunk
// inherits a clock another chunk already spent. The truncation split runs
// inside the call, which is why the split halves stay inside the same budget.
func (service *Service) reviewOneChunk(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	chunk diff.Chunk,
	pass *chunkPass,
) error {
	// Each call keeps its own accounting, so concurrent chunks never write
	// through one pointer, and the pass folds them in afterwards.
	var models modelSet
	requests := 0
	callCtx, cancel := context.WithTimeout(ctx, pass.settings.chunkTimeout)
	results, err := reviewChunk(
		callCtx,
		service.model,
		chunk,
		pass.settings.minimumImportance,
		pass.disputePrompt,
		&models,
		&requests,
		service.now,
	)
	cancel()
	pass.recordCall(models, requests, results)
	if err != nil {
		return err
	}

	findings := make([]domain.Finding, 0)
	for _, result := range results {
		findings = append(findings, result.Findings...)
	}
	return service.postChunkFindings(ctx, job, head, chunk.Text, findings, pass)
}

// postCandidate pairs one finding with its rendered comment, so ordering
// cannot separate the two.
type postCandidate struct {
	finding domain.Finding
	comment githubapp.InlineComment
}

// postChunkFindings posts every finding from one chunk that this run has not
// already carried. There is no cap: a reviewer does not ration comments, and
// the run stands behind every defect it reports.
//
// A finding is recorded as carried before its comment is attempted, so two
// chunks reporting the same defect post it once. A post that fails is reported
// to the caller, which leaves the whole chunk pending rather than advancing a
// checkpoint over findings the reader cannot see, unless GitHub answered and
// refused, which no later attempt can change.
func (service *Service) postChunkFindings(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	chunkText string,
	findings []domain.Finding,
	pass *chunkPass,
) error {
	logger := gklog.L(ctx)
	posts := service.chunkPosts(ctx, head, chunkText, findings, pass)
	if len(posts) == 0 {
		return nil
	}

	// Posting runs free of the caller's deadline. The findings are worth
	// nothing to the reader until they reach the pull request, so a stage that
	// ran long must not cancel the writes that deliver them.
	ctx, cancel := detachFromReviewDeadline(ctx, service.publicationTimeout)
	defer cancel()
	if err := service.confirmHead(ctx, job, head); err != nil {
		return err
	}

	// Each failed post is classified by its own error, never by the batch it
	// shares. A refusal is GitHub answering no, which no later attempt can
	// change, so it is final for that one comment. A transient failure is the
	// only thing that leaves the chunk pending: batching them together let one
	// dropped connection turn a refusal into an endless retry, because the
	// mixed batch never took the refused path.
	delivered := 0
	refused := 0
	var refusedErr error
	var transientErr error
	for _, post := range posts {
		err := service.github.CreateReviewComment(
			ctx,
			job.InstallationID,
			job.Repository,
			job.Number,
			head,
			post.comment,
		)
		if err != nil {
			logger.ErrorContext(
				ctx,
				"publish chunk finding",
				slog.String("path", post.comment.Path),
				slog.Int("line", post.comment.Line),
				slog.String("err", err.Error()),
			)
			pass.recordUndelivered()
			if commentRefusal(err) {
				refused++
				if refusedErr == nil {
					refusedErr = err
				}
			} else if transientErr == nil {
				transientErr = err
			}
			continue
		}
		pass.recordDelivered(post.finding)
		delivered++
	}
	logger.InfoContext(
		ctx,
		"review findings posted",
		slog.Int("offered", len(posts)),
		slog.Int("posted", delivered),
		slog.Int("refused", refused),
		slog.Int("failed", len(posts)-delivered),
	)
	if transientErr != nil {
		return fmt.Errorf("post chunk findings: %w", transientErr)
	}
	if refusedErr != nil {
		return fmt.Errorf("%w: %w", errCommentRefused, refusedErr)
	}
	return nil
}

// commentRefusal reports whether GitHub read this comment and rejected it for
// what it is, which no later attempt can change. A rate limit or a server
// error is GitHub failing to answer, not answering no: treating those as
// refusals checkpointed the chunk and silently dropped its findings, when the
// next run would have posted them fine.
//
// GitHub reports its primary and secondary rate limits as 403 as often as 429,
// so a 403 whose message says rate limit is a failure to answer too.
func commentRefusal(err error) bool {
	var apiErr githubapp.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode == http.StatusRequestTimeout {
		return false
	}
	if apiErr.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(apiErr.Message), "rate limit") {
		return false
	}
	return apiErr.StatusCode >= http.StatusBadRequest && apiErr.StatusCode < http.StatusInternalServerError
}

// recordDelivered records one finding whose comment reached the page.
func (pass *chunkPass) recordDelivered(finding domain.Finding) {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	pass.published = append(pass.published, finding)
	pass.posted++
}

// recordUndelivered records one finding whose comment did not reach the page.
// The count is what stops the run approving over a defect nobody can see.
func (pass *chunkPass) recordUndelivered() {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	pass.failed++
}

// renderChunkFindings turns the candidates a chunk still stands behind into the
// comments to post.
//
// It runs under the pass lock, and it tests suppression once more before
// rendering. The consolidation call between the two tests holds no lock, so
// another chunk can publish a claim in that window; testing again inside the
// same lock hold that records what this chunk carries is what keeps two chunks
// reporting one defect down to one comment.
func (service *Service) renderChunkFindings(
	ctx context.Context,
	head domain.HeadSHA,
	candidates []domain.Finding,
	pass *chunkPass,
) []postCandidate {
	logger := gklog.L(ctx)
	pass.mu.Lock()
	defer pass.mu.Unlock()

	posts := make([]postCandidate, 0, len(candidates))
	for _, finding := range unansweredCandidates(ctx, candidates, pass) {
		rendered, err := RenderInline(head, []domain.Finding{finding})
		if err != nil {
			// One finding that cannot be rendered says nothing about the others
			// in the chunk, so only it is dropped. It will never render, so the
			// chunk is finished rather than owed.
			logger.ErrorContext(
				ctx,
				"render chunk finding",
				slog.String("path", finding.Path),
				slog.String("err", err.Error()),
			)
			pass.failed++
			continue
		}
		pass.selection.remember(keysFor(finding), finding.Title)
		posts = append(posts, postCandidate{finding: finding, comment: rendered[0]})
	}
	return posts
}

// unansweredCandidates drops every candidate something already on the pull
// request carries: a thread still open on it, an earlier review, or an earlier
// chunk of this same run.
//
// The caller holds the pass lock, because the memory this reads is what makes
// two chunks reporting one defect post it once.
//
// It runs before the chunk's own answer is collapsed, and that order is load
// bearing. Collapsing first lets a candidate that is never going to be
// published take a live sibling into its group and down with it: one
// restatement of a settled thread, anchored across a range, swallowed a
// genuinely separate defect anchored inside that range and neither reached the
// page. Dropping the doomed candidates first leaves the collapse to decide only
// among candidates that could still be published.
func unansweredCandidates(
	ctx context.Context,
	eligible []domain.Finding,
	pass *chunkPass,
) []domain.Finding {
	unanswered := make([]domain.Finding, 0, len(eligible))
	for _, finding := range eligible {
		if match, answered := pass.disputes.answered(finding); answered {
			logSuppressed(ctx, layerOpenThreads, finding, match)
			continue
		}
		if layer, match, carried := pass.selection.suppressed(keysFor(finding)); carried {
			logSuppressed(ctx, layer, finding, match)
			continue
		}
		unanswered = append(unanswered, finding)
	}
	return unanswered
}

// confirmHead reports whether the pull request still points at the commit these
// findings describe.
//
// A read that fails is treated as current, because dropping real findings over
// one failed lookup costs the reader more than a comment on a commit that just
// moved. An expired context is not that case: the writes that follow would all
// fail while the findings stayed admitted as objections nobody can see, so a
// dead context is reported as the failure it is.
func (service *Service) confirmHead(ctx context.Context, job domain.ReviewJob, head domain.HeadSHA) error {
	logger := gklog.L(ctx)
	pullRequest, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if ctxErr := ctx.Err(); ctxErr != nil {
		logger.ErrorContext(ctx, "confirm head before posting findings", slog.String("err", ctxErr.Error()))
		return fmt.Errorf("confirm head: %w", ctxErr)
	}
	if err != nil {
		logger.ErrorContext(ctx, "read head before posting findings", slog.String("err", err.Error()))
		return nil
	}
	if pullRequest.Head != head {
		return errHeadMoved
	}
	return nil
}

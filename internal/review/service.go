package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/runlog"
)

const (
	checkSummaryFailure     = "Review failed."
	checkSummaryCancelled   = "Review cancelled."
	checkSummarySkipped     = "Review skipped."
	checkFailureReviews     = "Review failed while reading existing reviews."
	checkFailurePullRequest = "Review failed while loading the pull request."
	checkFailureReconcile   = "Review failed while reconciling existing findings."
	checkFailureDiff        = "Review failed while collecting the pull request diff."
	checkFailureSkip        = "Review failed while recording the skipped review."
	checkFailureAnalysis    = "Review failed during model analysis."
	checkFailureRefresh     = "Review failed while refreshing the pull request head."
	checkFailureThreads     = "Review failed while reading the open review threads."
	checkFailureSummary     = "Review failed while updating the visible summary."
	checkFailurePublish     = "Review failed while publishing the final decision."
	checkFailurePanic       = "Review failed after an internal panic."
	checkFailureUsage       = "Review stopped: the model provider reported no remaining usage."
	checkFailureDeadline    = "Review stopped: it ran out of time."
	// checkTitleAlreadyReviewed names a run that found nothing owed, whether
	// the durable state says so or an existing review marker does.
	checkTitleAlreadyReviewed = "Already reviewed"
	// checkConclusionDeclined is how a delta the admission gate refused ends.
	//
	// It is deliberately not "skipped". GitHub counts a required check concluded
	// skipped as passing, and an unreviewed delta must not merge on the strength
	// of having been declined. This conclusion holds the gate while the title
	// and the summary still report a skip rather than a failure.
	checkConclusionDeclined = "action_required"
	// completionBudget bounds the calls that finish the visible check.
	completionBudget = 30 * time.Second
	// publicationBudget bounds one batch of GitHub writes. Every such batch is
	// a handful of calls, so this is generous for the work while keeping a
	// stalled write from holding the run open.
	//
	// It is a fixed value rather than a slice of a review wide budget, because
	// there is no review wide budget: a run is bounded by admission, and the
	// only clock over a model call is the per chunk timeout.
	publicationBudget = 60 * time.Second
)

// Collector gathers pull request diff input for one review pass, scoped to the
// range since a previously reviewed commit when one is given.
type Collector interface {
	CollectRange(context.Context, domain.PullRequestRef, githubapp.PullRequest, domain.HeadSHA) (diff.ReviewInput, error)
}

// Reconciler silently resolves earlier bot findings on the current head.
type Reconciler interface {
	Reconcile(context.Context, domain.ReviewJob) ([]githubapp.ReviewThread, error)
}

// Service publishes one complete GitHub review per pull request head.
type Service struct {
	github            GitHub
	collector         Collector
	model             Model
	reconciler        Reconciler
	locker            *queue.KeyedLocker
	botLogin          string
	checkName         string
	minimumImportance int
	reviewMaxFiles    int
	reviewMaxChunks   int
	// chunkTimeout is the only clock over a model call, and the only clock in
	// a review at all. A run is bounded by admission instead, so a large diff
	// cannot run out of time part way through and lose what it already read.
	chunkTimeout           time.Duration
	checkCompletionTimeout time.Duration
	publicationTimeout     time.Duration
	now                    func() time.Time
	logger                 *slog.Logger
}

// NewService constructs a review publication service.
func NewService(
	github GitHub,
	collector Collector,
	model Model,
	reconciler Reconciler,
	locker *queue.KeyedLocker,
	botLogin string,
	minimumImportance int,
	reviewMaxFiles int,
	reviewMaxChunks int,
	chunkTimeout time.Duration,
	now func() time.Time,
	logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	// A non-positive budget refuses every real delta while admitting an empty
	// one, which is the opposite of what a budget is for. Treat it the same way
	// the configuration loader treats an unset variable.
	if reviewMaxFiles <= 0 {
		reviewMaxFiles = config.DefaultReviewMaxFiles
	}
	if reviewMaxChunks <= 0 {
		reviewMaxChunks = config.DefaultReviewMaxChunks
	}
	if chunkTimeout <= 0 {
		chunkTimeout = config.DefaultReviewChunkTimeout
	}
	return &Service{
		github:                 github,
		collector:              collector,
		model:                  model,
		reconciler:             reconciler,
		locker:                 locker,
		botLogin:               botLogin,
		checkName:              config.ReviewCheckName,
		minimumImportance:      minimumImportance,
		reviewMaxFiles:         reviewMaxFiles,
		reviewMaxChunks:        reviewMaxChunks,
		chunkTimeout:           chunkTimeout,
		checkCompletionTimeout: completionBudget,
		publicationTimeout:     publicationBudget,
		now:                    now,
		logger:                 logger,
	}
}

// Run reviews one pull request head and publishes the GitHub review lifecycle.
//
// Every log line this run writes is captured alongside the service log, and the
// capture is published in the check run body. A reader who opens a failed check
// therefore sees what the review did, not only the stage that failed.
//
// The run carries no deadline of its own. Admission is what bounds it, and the
// only clock inside it is the per chunk timeout, so a review can never run out
// of time part way through and lose the chunks it already read.
func (service *Service) Run(parent context.Context, job domain.ReviewJob) error {
	recorder := runlog.NewRecorder()
	logger := slog.New(runlog.Tee(service.logger.Handler(), recorder)).With(
		slog.String("delivery_id", job.DeliveryID),
		slog.String("repository", job.Repository.Owner+"/"+job.Repository.Name),
		slog.Int("pull_request", job.Number),
	)
	ctx := gklog.WithLogger(parent, logger)
	ctx = withRecorder(ctx, recorder)
	ctx = withShutdown(ctx, parent)
	logger.InfoContext(
		ctx,
		"review job started",
		slog.Int("minimum_importance", service.minimumImportance),
		slog.Duration("chunk_timeout", service.chunkTimeout),
	)
	if job.CheckRunID == 0 {
		return errors.New("review check was not admitted")
	}

	unlock := service.locker.Lock(job.Key())
	defer unlock()
	checkRun := githubapp.CheckRun{
		ID:         job.CheckRunID,
		Name:       service.checkName,
		Head:       job.Head,
		Status:     job.CheckRunStatus,
		Conclusion: job.CheckRunConclusion,
	}
	return service.runLocked(ctx, job, checkRun)
}

// Admit creates or resumes the visible check before background review work starts.
func (service *Service) Admit(parent context.Context, job domain.ReviewJob) (domain.ReviewJob, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), service.checkCompletionTimeout)
	defer cancel()
	logger := service.logger.With(
		slog.String("delivery_id", job.DeliveryID),
		slog.String("repository", job.Repository.Owner+"/"+job.Repository.Name),
		slog.Int("pull_request", job.Number),
		slog.String("head", string(job.Head)),
	)
	ctx = gklog.WithLogger(ctx, logger)

	checkRun, err := service.ensureCheckRun(ctx, job, job.Head)
	if err != nil {
		return job, err
	}
	job.CheckRunID = checkRun.ID
	job.CheckRunStatus = checkRun.Status
	job.CheckRunConclusion = checkRun.Conclusion
	logger.InfoContext(
		ctx,
		"review check admitted",
		slog.Int64("check_run_id", checkRun.ID),
		slog.String("status", checkRun.Status),
		slog.String("conclusion", checkRun.Conclusion),
	)
	return job, nil
}

// Reject completes an admitted check when background work cannot accept it.
func (service *Service) Reject(parent context.Context, job domain.ReviewJob, cause error) error {
	if job.CheckRunID == 0 || job.CheckRunStatus == "completed" {
		return nil
	}
	now := service.now()
	progress := service.newProgress(job.Head, now).summary(now)
	return service.failCheck(parent, job, job.CheckRunID, progress, checkSummaryFailure, cause)
}

func (service *Service) runLocked(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
) (runErr error) {
	logger := gklog.L(ctx)
	head := job.Head
	startedAt := service.now()
	progress := service.newProgress(head, startedAt)
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		panicErr := fmt.Errorf("review panic: %v", recovered)
		logger.ErrorContext(
			ctx,
			"review job panicked",
			slog.Any("panic", recovered),
			slog.String("stack", string(debug.Stack())),
			slog.String("err", panicErr.Error()),
		)
		runErr = service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailurePanic, panicErr)
	}()
	logger = logger.With(slog.String("head", string(head)))
	ctx = gklog.WithLogger(ctx, logger)

	if service.checkAlreadySucceeded(ctx, checkRun) {
		service.refreshVerdictAtReviewedHead(ctx, job, nil)
		return nil
	}
	pullRequest, err := service.github.GetPullRequest(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
	)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailurePullRequest, err)
	}
	if pullRequest.Head != head {
		return service.cancelCheck(ctx, job, checkRun.ID)
	}
	progress.reached("the pull request")

	reviews, reviewed, err := service.loadReviewHistory(ctx, job, head, progress)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureReviews, err)
	}
	if reviewed {
		service.refreshVerdictAtReviewedHead(ctx, job, reviews)
		return service.succeed(
			ctx,
			job,
			checkRun.ID,
			checkTitleAlreadyReviewed,
			"This head already has a PR-Agent review. No duplicate review was published.",
		)
	}
	return service.reviewOwedWork(ctx, job, checkRun, pullRequest, reviews, startedAt, progress)
}

// reviewOwedWork reviews everything this head still owes: the range since the
// last reviewed commit, plus whatever an earlier run left pending.
//
// The durable state is read here rather than at the top of the run, because
// every exit above it returns without a delta and would only pay for an issue
// comment read nothing uses.
func (service *Service) reviewOwedWork(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
	pullRequest githubapp.PullRequest,
	reviews []githubapp.Review,
	startedAt time.Time,
	progress *reviewProgress,
) error {
	head := job.Head
	state, hasState := service.loadDurableState(ctx, job)
	// Nothing is owed when the checkpoint already names this head with no chunk
	// pending. Deciding that here, rather than letting the collector compare a
	// commit against itself, spends no API call proving what the state already
	// says.
	if hasState && state.LastReviewed == head && len(state.Pending) == 0 {
		service.refreshVerdictAtReviewedHead(ctx, job, reviews)
		return service.succeed(
			ctx,
			job,
			checkRun.ID,
			checkTitleAlreadyReviewed,
			"The durable review state already records this head as reviewed, with no chunks pending.",
		)
	}

	// Admission runs before reconciliation. Reconciliation makes a model call
	// and resolves threads, so running it first would spend both on the exact
	// delta admission exists to refuse.
	work, stop, err := service.collectAndAdmit(
		ctx, job, pullRequest, checkRun, deltaBase(state, hasState), progress,
	)
	if stop {
		return err
	}

	threads, err := service.reconcileThreads(ctx, job, checkRun.ID, progress)
	if err != nil {
		return err
	}

	// Both are built from the threads reconciliation already loaded, so raising
	// an answered claim again costs the run no extra read and no extra model
	// call.
	selection := collectPublicationState(reviews, threads, service.botLogin)
	disputes := collectDisputes(threads, service.botLogin)
	pass := newChunkPass(work, service.minimumImportance, &selection, disputes)
	state, err = service.reviewDelta(ctx, job, head, state, pass)
	service.applyPass(ctx, pass, progress)
	if err != nil {
		if errors.Is(err, errHeadMoved) {
			return service.cancelCheck(ctx, job, checkRun.ID)
		}
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureAnalysis, err)
	}
	return service.publish(ctx, job, head, checkRun, reviews, pass, state, startedAt, progress)
}

// applyPass records what the chunk loop learned on the progress the failure and
// summary outputs both render from.
func (service *Service) applyPass(ctx context.Context, pass *chunkPass, progress *reviewProgress) {
	logger := gklog.L(ctx)
	analysis := pass.analysis()
	unread := pass.unreadChunks()
	posted, failed := pass.delivery()
	progress.applyAnalysis(analysis)
	progress.applyPublished(pass.publishedFindings())
	logChunkFailures(ctx, unread, len(pass.work.Chunks), pass.requestCount())
	logger.InfoContext(
		ctx,
		"review model analysis completed",
		slog.Int("chunks", len(pass.work.Chunks)),
		slog.Int("chunks_failed", len(unread)),
		slog.Int("comments_posted", posted),
		slog.Int("comments_undelivered", failed),
		slog.Bool("coverage_complete", analysis.CoverageComplete),
	)
	if err := logAnalysis(ctx, analysis); err != nil {
		logger.ErrorContext(ctx, "trace review analysis", slog.String("err", err.Error()))
	}
	progress.reached("model analysis")
}

// deltaBase names the commit the delta is measured from: the commit the last
// completed run reviewed, or nothing at all on first contact.
func deltaBase(state marker.State, hasState bool) domain.HeadSHA {
	if !hasState {
		return domain.HeadSHA("")
	}
	return state.LastReviewed
}

// publicationContext gives publication its own budget, freed from whatever the
// caller's context carries but still bound to the service lifetime.
func (service *Service) publicationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return detachFromReviewDeadline(ctx, service.publicationTimeout)
}

// detachFromReviewDeadline gives one stage of GitHub writes its own budget,
// freed from the caller's context but still bound to the service lifetime.
//
// Every stage that reports an outcome needs this: posting a chunk's findings,
// publishing the verdict, writing the failure notice, and completing the check.
// A cancelled or expired caller is exactly when the reader most needs the
// outcome, so it must not cancel the work that reports it. Shutdown still does,
// because a stopping service must not hold the process open writing to GitHub.
func detachFromReviewDeadline(
	ctx context.Context,
	budget time.Duration,
) (context.Context, context.CancelFunc) {
	shutdown := shutdownFrom(ctx)
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	if shutdown == nil {
		return detached, cancel
	}
	// A service already stopping never starts. Checking here rather than only in
	// the watch below removes the race where the first GitHub call would get
	// away before the watch observed the shutdown.
	if shutdown.Err() != nil {
		cancel()
		return detached, cancel
	}

	stopWatch := make(chan struct{})
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				gklog.L(detached).ErrorContext(
					detached,
					"shutdown watch panicked",
					slog.Any("panic", recovered),
					slog.String("err", "shutdown watch panicked"),
				)
			}
		}()
		select {
		case <-shutdown.Done():
			cancel()
		case <-stopWatch:
		}
	}()
	return detached, func() {
		close(stopWatch)
		cancel()
	}
}

// shutdownKey names the service lifetime context carried alongside the review.
type shutdownKey struct{}

// withShutdown carries the service lifetime context, so a later stage can free
// itself from the review deadline without also freeing itself from shutdown.
func withShutdown(ctx context.Context, shutdown context.Context) context.Context {
	return context.WithValue(ctx, shutdownKey{}, shutdown)
}

// shutdownFrom returns the service lifetime context, or nil when none was set.
func shutdownFrom(ctx context.Context) context.Context {
	shutdown, ok := ctx.Value(shutdownKey{}).(context.Context)
	if !ok {
		return nil
	}
	return shutdown
}

// loadReviewHistory reads every prior review and reports whether this head was
// already reviewed, which is how a redelivered webhook avoids a second review.
func (service *Service) loadReviewHistory(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	progress *reviewProgress,
) ([]githubapp.Review, bool, error) {
	logger := gklog.L(ctx)
	reviews, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "list reviews", slog.String("err", err.Error()))
		return nil, false, fmt.Errorf("list reviews: %w", err)
	}
	progress.priorReviews = traceReviews(reviews, service.botLogin)
	progress.reached("the review history")
	logger.InfoContext(
		ctx,
		"review history loaded",
		slog.Any("bot_reviews", progress.priorReviews),
	)
	if hasBotReviewMarker(reviews, service.botLogin, head) {
		logger.InfoContext(ctx, "review job suppressed", slog.String("reason", "review_marker"))
		return reviews, true, nil
	}
	return reviews, false, nil
}

// reconcileThreads silently resolves earlier bot findings on the current head
// and reports what survives, returning the check failure directly so the
// caller does not repeat the two-step report-and-return pattern.
func (service *Service) reconcileThreads(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	progress *reviewProgress,
) ([]githubapp.ReviewThread, error) {
	logger := gklog.L(ctx)
	threads, err := service.reconciler.Reconcile(ctx, job)
	if err != nil {
		return nil, service.failCheck(ctx, job, checkRunID, progress.summary(service.now()), checkFailureReconcile, err)
	}
	progress.threads = traceThreads(threads, service.botLogin)
	progress.reached("thread reconciliation")
	logger.InfoContext(ctx, "review threads reconciled", slog.Any("bot_threads", progress.threads))
	return threads, nil
}

func (service *Service) newProgress(head domain.HeadSHA, startedAt time.Time) *reviewProgress {
	return newReviewProgress(head, startedAt, service.minimumImportance)
}

// checkAlreadySucceeded reports whether this head already carries a completed
// successful check, which means the review ran and needs no repeat.
func (service *Service) checkAlreadySucceeded(ctx context.Context, checkRun githubapp.CheckRun) bool {
	logger := gklog.L(ctx)
	logger.InfoContext(
		ctx,
		"review check loaded",
		slog.Int64("check_run_id", checkRun.ID),
		slog.String("status", checkRun.Status),
		slog.String("conclusion", checkRun.Conclusion),
	)
	if checkRun.Status != "completed" || checkRun.Conclusion != "success" {
		return false
	}
	logger.InfoContext(ctx, "review job suppressed", slog.String("reason", "completed_check"))
	return true
}

func logAnalysis(ctx context.Context, analysis Analysis) error {
	logger := gklog.L(ctx)
	observedTrace, err := traceFindings(ctx, analysis.Observed)
	if err != nil {
		logger.ErrorContext(ctx, "trace observed findings", slog.String("err", err.Error()))
		return fmt.Errorf("trace observed findings: %w", err)
	}
	eligibleTrace, err := traceFindings(ctx, analysis.Anchored)
	if err != nil {
		logger.ErrorContext(ctx, "trace eligible findings", slog.String("err", err.Error()))
		return fmt.Errorf("trace eligible findings: %w", err)
	}
	logger.InfoContext(
		ctx,
		"review analysis classified",
		slog.Bool("coverage_complete", analysis.CoverageComplete),
		slog.String("decision", string(analysis.Decision)),
		slog.Any("observed_findings", observedTrace),
		slog.Any("eligible_findings", eligibleTrace),
	)
	return nil
}

// publish closes out a review that read every chunk it owed. It reads both
// verdict inputs after this run's findings are on the page, and submits nothing
// at all when the head has moved on.
func (service *Service) publish(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	checkRun githubapp.CheckRun,
	reviews []githubapp.Review,
	pass *chunkPass,
	state marker.State,
	startedAt time.Time,
	progress *reviewProgress,
) error {
	ctx, cancelPublication := service.publicationContext(ctx)
	defer cancelPublication()

	// Reading a commit proves it was reviewed, not that it is still the head. A
	// verdict submitted here would judge a commit this run never read, so a
	// moved head ends the run and leaves the work to the push that moved it.
	currentPullRequest, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureRefresh, err)
	}
	if currentPullRequest.Head != head {
		return service.cancelCheck(ctx, job, checkRun.ID)
	}
	progress.reached("the head refresh")

	// The findings already reached the pull request as their chunks answered, so
	// the review submitted here carries the verdict and the summary alone.
	analysis := pass.analysis()
	published := pass.publishedFindings()
	posted, failed := pass.delivery()
	logPublishedFindings(ctx, analysis.Anchored, published, posted, failed)
	progress.reached("finding selection")

	threads, err := service.openThreads(ctx, job, progress)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureThreads, err)
	}

	// A head is fully reviewed only when every chunk it owed answered, every
	// chunk covered its whole hunk, and every finding this run stands behind
	// reached the page. A finding whose comment GitHub refused leaves the reader
	// nothing to act on, so the run must not approve over it.
	headFullyReviewed := len(state.Pending) == 0 && analysis.CoverageComplete && failed == 0
	summary := Summary{
		Head:              head,
		Decision:          reviewerDecision(threads, service.botLogin, headFullyReviewed),
		Blocking:          blockingReasons(threads, service.botLogin, job.PullRequestRef, headFullyReviewed),
		Models:            analysis.Models,
		Duration:          service.now().Sub(startedAt),
		FilesReviewed:     analysis.FilesReviewed,
		Chunks:            analysis.Chunks,
		CoverageComplete:  analysis.CoverageComplete,
		MinimumImportance: service.minimumImportance,
		Observed:          analysis.Observed,
		Eligible:          analysis.Anchored,
		Published:         published,
		PriorReviews:      traceReviews(reviews, service.botLogin),
		Threads:           traceThreads(threads, service.botLogin),
		Reached:           "",
		Failed:            false,
	}
	if len(state.Pending) > 0 {
		return service.concludeIncomplete(ctx, job, checkRun, state, pass, summary, progress)
	}
	return service.publishVerdict(ctx, job, checkRun, summary, state, progress)
}

// openThreads reads the service's own threads as they stand now, which is one
// of the two inputs the verdict is computed from.
//
// It runs after this run's findings are posted. A snapshot taken before
// analysis omits every thread this run just opened, and a verdict computed
// from it would approve over defects the same run had raised minutes earlier.
func (service *Service) openThreads(
	ctx context.Context,
	job domain.ReviewJob,
	progress *reviewProgress,
) ([]githubapp.ReviewThread, error) {
	logger := gklog.L(ctx)
	threads, err := service.github.ListReviewThreads(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "list review threads for the verdict", slog.String("err", err.Error()))
		return nil, fmt.Errorf("list review threads: %w", err)
	}
	progress.threads = traceThreads(threads, service.botLogin)
	progress.reached("the thread refresh")
	logger.InfoContext(ctx, "verdict threads loaded", slog.Any("bot_threads", progress.threads))
	return threads, nil
}

// publishVerdict writes the verdict this run computed: the review carrying the
// decision, the one top level comment carrying the summary and the durable
// state, and the completed check.
func (service *Service) publishVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
	summary Summary,
	state marker.State,
	progress *reviewProgress,
) error {
	logger := gklog.L(ctx)
	publishedReview, err := service.github.SubmitReview(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
		githubapp.SubmitReviewRequest{
			CommitID: summary.Head,
			Body:     RenderVerdictBody(summary),
			Event:    summary.Decision,
			Comments: nil,
		},
	)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailurePublish, err)
	}
	logger.InfoContext(
		ctx,
		"review published",
		slog.Int64("review_id", publishedReview.ID),
		slog.String("event", string(summary.Decision)),
		slog.Int("streamed_comments", len(summary.Published)),
		slog.Any("blocking", summary.Blocking),
	)

	// The one top level comment carries the same body plus the durable state
	// the chunk loop advanced, so a later invocation resumes from a checkpoint
	// this run actually reached rather than one it asserted.
	if err := service.upsertSummaryComment(ctx, job, summaryCommentContent{
		Prose: RenderBody(summary),
		State: state,
	}); err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureSummary, err)
	}

	if err := service.succeed(ctx, job, checkRun.ID, summary.Title(), RenderDetails(summary)); err != nil {
		return err
	}
	logger.InfoContext(ctx, "review job completed", slog.Int64("check_run_id", checkRun.ID))
	return nil
}

func (service *Service) ensureCheckRun(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
) (githubapp.CheckRun, error) {
	logger := gklog.L(ctx)
	checkRun, found, err := service.github.FindCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		head,
		service.checkName,
	)
	if err != nil {
		logger.ErrorContext(ctx, "find check run", slog.String("err", err.Error()))
		return githubapp.CheckRun{}, fmt.Errorf("find check run: %w", err)
	}
	if !found {
		checkRun, err = service.github.CreateCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			head,
			service.checkName,
		)
		if err != nil {
			logger.ErrorContext(ctx, "create check run", slog.String("err", err.Error()))
			return githubapp.CheckRun{}, fmt.Errorf("create check run: %w", err)
		}
	}
	if checkRun.Status == "queued" {
		if err := service.github.StartCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			checkRun.ID,
			repositoryURL(job.Repository),
		); err != nil {
			logger.ErrorContext(ctx, "start check run", slog.String("err", err.Error()))
			return githubapp.CheckRun{}, fmt.Errorf("start check run: %w", err)
		}
		checkRun.Status = "in_progress"
	}
	return checkRun, nil
}

func (service *Service) succeed(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	title string,
	summary string,
) error {
	logger := gklog.L(ctx)
	if err := service.completeCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		checkRunID,
		"success",
		title,
		summary,
	); err != nil {
		logger.ErrorContext(ctx, "complete successful check run", slog.String("err", err.Error()))
		return fmt.Errorf("complete check run: %w", err)
	}
	return nil
}

func (service *Service) cancelCheck(ctx context.Context, job domain.ReviewJob, checkRunID int64) error {
	logger := gklog.L(ctx)
	if err := service.completeCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		checkRunID,
		"cancelled",
		checkSummaryCancelled,
		checkSummaryCancelled,
	); err != nil {
		logger.ErrorContext(ctx, "complete cancelled check run", slog.String("err", err.Error()))
		return fmt.Errorf("complete cancelled check run: %w", err)
	}
	return nil
}

func (service *Service) completeCheckRun(
	ctx context.Context,
	installationID int64,
	repository domain.Repository,
	checkRunID int64,
	conclusion string,
	title string,
	summary string,
) error {
	logger := gklog.L(ctx)
	completionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), service.checkCompletionTimeout)
	defer cancel()
	// The log is rendered before the completion call, so the published text is
	// everything the run recorded up to the moment it finished.
	err := service.github.CompleteCheckRun(
		completionCtx,
		installationID,
		repository,
		checkRunID,
		conclusion,
		title,
		summary,
		renderRunLog(ctx),
	)
	if err != nil {
		logger.ErrorContext(ctx, "complete check run", slog.String("err", err.Error()))
		return fmt.Errorf("complete check run: %w", err)
	}
	return nil
}

func repositoryURL(repo domain.Repository) string {
	return (&url.URL{
		Scheme: "https",
		Host:   "github.com",
		Path:   "/" + repo.Owner + "/" + repo.Name,
	}).String()
}

// refreshVerdictAtReviewedHead reconciles the standing verdict with current
// thread state at a head that is already reviewed, submitting a fresh verdict
// review only when the two disagree. A pull_request_review_thread delivery
// lands here, which is what lets resolving a thread unblock a pull request
// with no push. Failures are logged rather than failing the run: the head is
// reviewed, and the check must keep saying so.
//
// reviews is the review list the caller already loaded, or nil when the run
// exited before loading one.
func (service *Service) refreshVerdictAtReviewedHead(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
) {
	logger := gklog.L(ctx)
	ctx, cancel := service.publicationContext(ctx)
	defer cancel()
	if reviews == nil {
		listed, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
		if err != nil {
			logger.ErrorContext(ctx, "list reviews for verdict refresh", slog.String("err", err.Error()))
			return
		}
		reviews = listed
	}
	verdict, found := latestBotVerdictReview(reviews, service.botLogin)
	if !found {
		return
	}
	threads, err := service.github.ListReviewThreads(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "list threads for verdict refresh", slog.String("err", err.Error()))
		return
	}
	// The standing verdict says whether its run fully reviewed the head. A
	// thread resolution changes nothing about how much was reviewed, so that
	// part of the block must survive the refresh.
	headFullyReviewed := !strings.Contains(verdict.Body, unreviewedHeadReason)
	decision := reviewerDecision(threads, service.botLogin, headFullyReviewed)
	if reviewStateFor(decision) == verdict.State {
		return
	}
	service.submitRefreshedVerdict(ctx, job, decision, threads, headFullyReviewed)
}

// submitRefreshedVerdict publishes the verdict a refresh computed: a fresh
// verdict review at the reviewed head plus the summary comment prose, with the
// durable state kept exactly as the last completed run wrote it.
func (service *Service) submitRefreshedVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	decision domain.ReviewDecision,
	threads []githubapp.ReviewThread,
	headFullyReviewed bool,
) {
	logger := gklog.L(ctx)
	// A verdict submitted at a moved head would judge a commit nobody is
	// looking at any more, so the refresh checks the head one last time.
	currentPullRequest, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "refresh verdict head check", slog.String("err", err.Error()))
		return
	}
	if currentPullRequest.Head != job.Head {
		logger.InfoContext(ctx, "verdict refresh skipped", slog.String("reason", "head_moved"))
		return
	}
	summary := Summary{
		Head:              job.Head,
		Decision:          decision,
		Blocking:          blockingReasons(threads, service.botLogin, job.PullRequestRef, headFullyReviewed),
		Models:            nil,
		Duration:          0,
		FilesReviewed:     0,
		Chunks:            0,
		CoverageComplete:  headFullyReviewed,
		MinimumImportance: service.minimumImportance,
		Observed:          nil,
		Eligible:          nil,
		Published:         nil,
		PriorReviews:      nil,
		Threads:           traceThreads(threads, service.botLogin),
		Reached:           "",
		Failed:            false,
	}
	submitted, err := service.github.SubmitReview(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
		githubapp.SubmitReviewRequest{
			CommitID: job.Head,
			Body:     RenderVerdictBody(summary),
			Event:    decision,
			Comments: nil,
		},
	)
	if err != nil {
		logger.ErrorContext(ctx, "submit refreshed verdict", slog.String("err", err.Error()))
		return
	}
	logger.InfoContext(
		ctx,
		"review verdict refreshed",
		slog.Int64("review_id", submitted.ID),
		slog.String("event", string(decision)),
		slog.Any("blocking", summary.Blocking),
	)
	if err := service.upsertSummaryCommentFrom(ctx, job, func(state marker.State) summaryCommentContent {
		return summaryCommentContent{Prose: renderVerdictRefreshProse(summary), State: state}
	}); err != nil {
		logger.ErrorContext(ctx, "update summary after verdict refresh", slog.String("err", err.Error()))
	}
}

func hasBotReviewMarker(
	reviews []githubapp.Review,
	botLogin string,
	head domain.HeadSHA,
) bool {
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		if item.CommitID != head {
			continue
		}
		markerHead, ok := marker.FindReview(item.Body)
		if !ok {
			continue
		}
		if markerHead == head {
			return true
		}
	}
	return false
}

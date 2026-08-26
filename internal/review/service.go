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
	checkSummaryFailure      = "Review failed."
	checkSummaryCancelled    = "Review cancelled."
	checkFailureReviews      = "Review failed while reading existing reviews."
	checkFailurePullRequest  = "Review failed while loading the pull request."
	checkFailureReconcile    = "Review failed while reconciling existing findings."
	checkFailureDiff         = "Review failed while collecting the pull request diff."
	checkFailureAnalysis     = "Review failed during model analysis."
	checkFailureRefresh      = "Review failed while refreshing the pull request head."
	checkFailureRender       = "Review failed while rendering inline findings."
	checkFailureSummary      = "Review failed while updating the visible summary."
	checkFailurePublish      = "Review failed while publishing the final decision."
	checkFailurePanic        = "Review failed after an internal panic."
	checkFailureUsage        = "Review stopped: the model provider reported no remaining usage."
	maxCheckFailureRunes     = 1000
	maximumCompletionTimeout = 30 * time.Second
	// maximumPublicationTimeout caps the slice of the review budget reserved for
	// publishing. Publication is a handful of GitHub calls, so this is generous
	// for the work while staying small against a ten minute review.
	maximumPublicationTimeout = 60 * time.Second
)

// GitHub loads pull request state and publishes review lifecycle updates.
type GitHub interface {
	GetPullRequest(context.Context, int64, domain.Repository, int) (githubapp.PullRequest, error)
	ListReviews(context.Context, int64, domain.Repository, int) ([]githubapp.Review, error)
	FindCheckRun(context.Context, int64, domain.Repository, domain.HeadSHA, string) (githubapp.CheckRun, bool, error)
	CreateCheckRun(context.Context, int64, domain.Repository, domain.HeadSHA, string) (githubapp.CheckRun, error)
	StartCheckRun(context.Context, int64, domain.Repository, int64, string) error
	CompleteCheckRun(context.Context, int64, domain.Repository, int64, string, string, string, string) error
	SubmitReview(context.Context, int64, domain.Repository, int, githubapp.SubmitReviewRequest) (githubapp.Review, error)
	UpdateReview(context.Context, int64, domain.Repository, int, int64, string) (githubapp.Review, error)
	DismissReview(context.Context, int64, domain.Repository, int, int64, string) error
}

// Collector gathers pull request diff input for one review pass.
type Collector interface {
	Collect(context.Context, domain.PullRequestRef, githubapp.PullRequest) (diff.ReviewInput, error)
}

// Reconciler silently resolves earlier bot findings on the current head.
type Reconciler interface {
	Reconcile(context.Context, domain.ReviewJob) ([]githubapp.ReviewThread, error)
}

// Service publishes one complete GitHub review per pull request head.
type Service struct {
	github                    GitHub
	collector                 Collector
	model                     Model
	reconciler                Reconciler
	locker                    *queue.KeyedLocker
	botLogin                  string
	checkName                 string
	minimumImportance         int
	maximumUnresolvedComments int
	reviewTimeout             time.Duration
	checkCompletionTimeout    time.Duration
	publicationTimeout        time.Duration
	now                       func() time.Time
	logger                    *slog.Logger
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
	maximumUnresolvedComments int,
	reviewTimeout time.Duration,
	now func() time.Time,
	logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	// Publishing the review and completing the check each get a reserved slice
	// of the budget, and analysis gets what remains. Reaching the analysis
	// deadline then still leaves time to publish what the review already found,
	// so a slow diff produces a partial review rather than nothing at all.
	completionTimeout := min(reviewTimeout/4, maximumCompletionTimeout)
	publicationTimeout := min(reviewTimeout/4, maximumPublicationTimeout)
	return &Service{
		github:                    github,
		collector:                 collector,
		model:                     model,
		reconciler:                reconciler,
		locker:                    locker,
		botLogin:                  botLogin,
		checkName:                 config.ReviewCheckName,
		minimumImportance:         minimumImportance,
		maximumUnresolvedComments: maximumUnresolvedComments,
		reviewTimeout:             reviewTimeout - completionTimeout - publicationTimeout,
		checkCompletionTimeout:    completionTimeout,
		publicationTimeout:        publicationTimeout,
		now:                       now,
		logger:                    logger,
	}
}

// Run reviews one pull request head and publishes the GitHub review lifecycle.
//
// Every log line this run writes is captured alongside the service log, and the
// capture is published in the check run body. A reader who opens a failed check
// therefore sees what the review did, not only the stage that failed.
func (service *Service) Run(parent context.Context, job domain.ReviewJob) error {
	ctx, cancel := context.WithTimeout(parent, service.reviewTimeout)
	defer cancel()
	recorder := runlog.NewRecorder()
	logger := slog.New(runlog.Tee(service.logger.Handler(), recorder)).With(
		slog.String("delivery_id", job.DeliveryID),
		slog.String("repository", job.Repository.Owner+"/"+job.Repository.Name),
		slog.Int("pull_request", job.Number),
	)
	ctx = gklog.WithLogger(ctx, logger)
	ctx = withRecorder(ctx, recorder)
	logger.InfoContext(
		ctx,
		"review job started",
		slog.Int("minimum_importance", service.minimumImportance),
		slog.Int("maximum_unresolved_comments", service.maximumUnresolvedComments),
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

	reviews, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureReviews, err)
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
		return service.succeed(
			ctx,
			job,
			checkRun.ID,
			"Already reviewed",
			"This head already has a PR-Agent review. No duplicate review was published.",
		)
	}

	threads, err := service.reconciler.Reconcile(ctx, job)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureReconcile, err)
	}
	progress.threads = traceThreads(threads, service.botLogin)
	progress.reached("thread reconciliation")
	logger.InfoContext(
		ctx,
		"review threads reconciled",
		slog.Any("bot_threads", progress.threads),
	)

	input, err := service.collector.Collect(ctx, job.PullRequestRef, pullRequest)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureDiff, err)
	}
	progress.reached("the diff")

	analysis, err := Analyze(ctx, service.model, input, service.minimumImportance, service.now)
	progress.applyAnalysis(analysis)
	if err != nil {
		// A chunk that panicked is an internal fault, not a model failure, and
		// the reported cause has to say so.
		title := checkFailureAnalysis
		if isChunkPanic(err) {
			title = checkFailurePanic
		}
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), title, err)
	}
	progress.reached("model analysis")
	if err := logAnalysis(ctx, analysis); err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureAnalysis, err)
	}

	return service.publish(ctx, job, head, checkRun, reviews, threads, analysis, startedAt, progress)
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

func (service *Service) publish(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	checkRun githubapp.CheckRun,
	reviews []githubapp.Review,
	threads []githubapp.ReviewThread,
	analysis Analysis,
	startedAt time.Time,
	progress *reviewProgress,
) error {
	// Publication runs on its own reserved budget, detached from the analysis
	// deadline. Analysis is what runs long, and the findings it produced are
	// worth nothing to the reader until they reach the pull request, so its
	// deadline must not be able to cancel the publish that follows it.
	ctx, cancelPublication := context.WithTimeout(
		context.WithoutCancel(ctx),
		service.publicationTimeout,
	)
	defer cancelPublication()

	logger := gklog.L(ctx)
	currentPullRequest, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureRefresh, err)
	}
	if currentPullRequest.Head != head {
		return service.cancelCheck(ctx, job, checkRun.ID)
	}
	progress.reached("the head refresh")

	publishedFindings, err := selectFindingsForPublication(
		ctx,
		analysis.Anchored,
		reviews,
		threads,
		service.botLogin,
		service.maximumUnresolvedComments,
	)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureRender, err)
	}

	comments, err := RenderInline(head, publishedFindings)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureRender, err)
	}
	progress.reached("finding selection")

	decision := standingDecision(ctx, analysis, publishedFindings, threads, service.botLogin)

	summary := Summary{
		Head:              head,
		Decision:          decision,
		Models:            analysis.Models,
		Duration:          service.now().Sub(startedAt),
		FilesReviewed:     analysis.FilesReviewed,
		Chunks:            analysis.Chunks,
		CoverageComplete:  analysis.CoverageComplete,
		MinimumImportance: service.minimumImportance,
		Observed:          analysis.Observed,
		Eligible:          analysis.Anchored,
		Published:         publishedFindings,
		PriorReviews:      traceReviews(reviews, service.botLogin),
		Threads:           traceThreads(threads, service.botLogin),
		Reached:           "",
		Failed:            false,
	}

	body, err := service.prepareReviewBody(ctx, job, reviews, summary)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureSummary, err)
	}

	publishedReview, err := service.github.SubmitReview(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
		githubapp.SubmitReviewRequest{
			CommitID: head,
			Body:     body,
			Event:    decision,
			Comments: comments,
		},
	)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailurePublish, err)
	}
	logger.InfoContext(
		ctx,
		"review published",
		slog.Int64("review_id", publishedReview.ID),
		slog.String("event", string(decision)),
		slog.Int("inline_comments", len(comments)),
		slog.Bool("visible_body", true),
	)

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

func formatFindingImportances(findings []domain.Finding) string {
	if len(findings) == 0 {
		return "none"
	}
	values := make([]string, 0, len(findings))
	for _, finding := range findings {
		values = append(values, fmt.Sprintf("`%d`", finding.Importance))
	}
	return strings.Join(values, ", ")
}

func formatReviewTraceIDs(reviews []reviewTrace) string {
	if len(reviews) == 0 {
		return "none"
	}
	ids := make([]string, 0, len(reviews))
	for _, item := range reviews {
		ids = append(ids, fmt.Sprintf("`%d`", item.ID))
	}
	return strings.Join(ids, ", ")
}

func formatThreadTraceIDs(threads []threadTrace) string {
	if len(threads) == 0 {
		return "none"
	}
	ids := make([]string, 0, len(threads))
	for _, item := range threads {
		ids = append(ids, "`"+item.NodeID+"`")
	}
	return strings.Join(ids, ", ")
}

func countResolvedThreadTraces(threads []threadTrace) int {
	count := 0
	for _, item := range threads {
		if item.Resolved {
			count++
		}
	}
	return count
}

func (service *Service) failCheck(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	progress Summary,
	title string,
	cause error,
) error {
	logger := gklog.L(ctx)
	if title == "" {
		title = checkSummaryFailure
	}
	if usageExceeded(cause) {
		title = checkFailureUsage
	}
	progress.Failed = true
	detail := checkFailureDetail(cause)
	// The check run reports the same cause and the same progress the comment
	// carries, so both explain the failure rather than only naming a stage.
	checkSummary := detail + "\n\n" + RenderDetails(progress)
	var completeErr error
	if checkRunID != 0 {
		completeErr = service.completeCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			checkRunID,
			"failure",
			title,
			checkSummary,
		)
		if completeErr != nil {
			logger.ErrorContext(ctx, "complete failed check run", slog.String("err", completeErr.Error()))
		}
	}
	service.clearFailedReviewState(ctx, job, progress, title, detail)
	if completeErr != nil {
		return fmt.Errorf("complete check run: %w", completeErr)
	}
	logger.ErrorContext(ctx, "review job failed", slog.String("err", cause.Error()))
	return cause
}

// clearFailedReviewState leaves the pull request in a state a reader can act
// on. A failed review has no verdict, so any verdict the service left earlier
// is withdrawn, and the visible summary says why the review stopped.
//
// Neither step can mask the reported cause, so a failure here is logged and the
// remaining work still runs.
func (service *Service) clearFailedReviewState(
	ctx context.Context,
	job domain.ReviewJob,
	progress Summary,
	title string,
	detail string,
) {
	logger := gklog.L(ctx)
	reviews, err := service.listReviewsForFailure(ctx, job)
	if err != nil {
		return
	}
	if dismissErr := service.dismissStaleVerdicts(ctx, job, reviews); dismissErr != nil {
		logger.ErrorContext(ctx, "dismiss stale verdicts", slog.String("err", dismissErr.Error()))
	}
	service.publishFailureNotice(ctx, job, reviews, progress, title, detail)
}

// usageExceededError is any provider error that reports exhausted usage.
type usageExceededError interface {
	UsageExceeded() bool
}

func usageExceeded(cause error) bool {
	var target usageExceededError
	if !errors.As(cause, &target) {
		return false
	}
	return target.UsageExceeded()
}

func (service *Service) prepareReviewBody(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
	summary Summary,
) (string, error) {
	body := RenderBody(summary)
	updated, err := service.updateSummaryReview(ctx, job, reviews, body)
	if err != nil {
		return "", err
	}
	if updated {
		return marker.Review(summary.Head), nil
	}
	return body, nil
}

// updateSummaryReview replaces the single visible summary body in place and
// reports whether one existed to replace.
func (service *Service) updateSummaryReview(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
	body string,
) (bool, error) {
	logger := gklog.L(ctx)
	summaryReview, found := findSummaryReview(reviews, service.botLogin)
	if !found {
		return false, nil
	}
	if _, err := service.github.UpdateReview(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
		summaryReview.ID,
		body,
	); err != nil {
		logger.ErrorContext(ctx, "update review summary", slog.String("err", err.Error()))
		return false, fmt.Errorf("update review summary: %w", err)
	}
	logger.InfoContext(
		ctx,
		"review summary updated",
		slog.Int64("review_id", summaryReview.ID),
		slog.Bool("visible", true),
	)
	return true, nil
}

func findSummaryReview(reviews []githubapp.Review, botLogin string) (githubapp.Review, bool) {
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		if marker.HasSummary(item.Body) {
			return item, true
		}
	}
	return githubapp.Review{
		ID:       0,
		CommitID: "",
		Author:   "",
		Body:     "",
		State:    "",
	}, false
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

func checkFailureDetail(cause error) string {
	if cause == nil {
		return "No failure detail was reported."
	}
	detail := strings.Join(strings.Fields(cause.Error()), " ")
	if detail == "" {
		return "No failure detail was reported."
	}
	detailRunes := []rune(detail)
	if len(detailRunes) > maxCheckFailureRunes {
		detail = string(detailRunes[:maxCheckFailureRunes-3]) + "..."
	}
	return detail
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

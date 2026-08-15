package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/queue"
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
	maxCheckFailureRunes     = 1000
	maximumCompletionTimeout = 30 * time.Second
)

// GitHub loads pull request state and publishes review lifecycle updates.
type GitHub interface {
	GetPullRequest(context.Context, int64, domain.Repository, int) (githubapp.PullRequest, error)
	ListReviews(context.Context, int64, domain.Repository, int) ([]githubapp.Review, error)
	FindCheckRun(context.Context, int64, domain.Repository, domain.HeadSHA, string) (githubapp.CheckRun, bool, error)
	CreateCheckRun(context.Context, int64, domain.Repository, domain.HeadSHA, string) (githubapp.CheckRun, error)
	StartCheckRun(context.Context, int64, domain.Repository, int64, string) error
	CompleteCheckRun(context.Context, int64, domain.Repository, int64, string, string, string) error
	SubmitReview(context.Context, int64, domain.Repository, int, githubapp.SubmitReviewRequest) (githubapp.Review, error)
	UpdateReview(context.Context, int64, domain.Repository, int, int64, string) (githubapp.Review, error)
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
	logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	completionTimeout := min(reviewTimeout/4, maximumCompletionTimeout)
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
		reviewTimeout:             reviewTimeout - completionTimeout,
		checkCompletionTimeout:    completionTimeout,
		logger:                    logger,
	}
}

// Run reviews one pull request head and publishes the GitHub review lifecycle.
func (service *Service) Run(parent context.Context, job domain.ReviewJob) error {
	ctx, cancel := context.WithTimeout(parent, service.reviewTimeout)
	defer cancel()
	logger := service.logger.With(
		slog.String("delivery_id", job.DeliveryID),
		slog.String("repository", job.Repository.Owner+"/"+job.Repository.Name),
		slog.Int("pull_request", job.Number),
	)
	ctx = gklog.WithLogger(ctx, logger)
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
	return service.failCheck(parent, job, job.CheckRunID, checkSummaryFailure, cause)
}

func (service *Service) runLocked(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
) (runErr error) {
	logger := gklog.L(ctx)
	head := job.Head
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
		runErr = service.failCheck(ctx, job, checkRun.ID, checkFailurePanic, panicErr)
	}()
	logger = logger.With(slog.String("head", string(head)))
	ctx = gklog.WithLogger(ctx, logger)
	logger.InfoContext(
		ctx,
		"review check loaded",
		slog.Int64("check_run_id", checkRun.ID),
		slog.String("status", checkRun.Status),
		slog.String("conclusion", checkRun.Conclusion),
	)
	if checkRun.Status == "completed" && checkRun.Conclusion == "success" {
		logger.InfoContext(ctx, "review job suppressed", slog.String("reason", "completed_check"))
		return nil
	}
	pullRequest, err := service.github.GetPullRequest(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
	)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailurePullRequest, err)
	}
	if pullRequest.Head != head {
		return service.cancelCheck(ctx, job, checkRun.ID)
	}

	reviews, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureReviews, err)
	}
	logger.InfoContext(
		ctx,
		"review history loaded",
		slog.Any("bot_reviews", traceReviews(reviews, service.botLogin)),
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
		return service.failCheck(ctx, job, checkRun.ID, checkFailureReconcile, err)
	}
	logger.InfoContext(
		ctx,
		"review threads reconciled",
		slog.Any("bot_threads", traceThreads(threads, service.botLogin)),
	)

	input, err := service.collector.Collect(ctx, job.PullRequestRef, pullRequest)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureDiff, err)
	}

	analysis, err := Analyze(ctx, service.model, input, service.minimumImportance)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureAnalysis, err)
	}
	if err := logAnalysis(ctx, analysis); err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureAnalysis, err)
	}

	return service.publish(ctx, job, head, checkRun, reviews, threads, analysis)
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
) error {
	logger := gklog.L(ctx)
	currentPullRequest, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureRefresh, err)
	}
	if currentPullRequest.Head != head {
		return service.cancelCheck(ctx, job, checkRun.ID)
	}

	publishedFindings, err := selectFindingsForPublication(
		ctx,
		analysis.Anchored,
		reviews,
		threads,
		service.botLogin,
		service.maximumUnresolvedComments,
	)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureRender, err)
	}

	comments, err := RenderInline(head, publishedFindings)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureRender, err)
	}

	body, err := service.prepareReviewBody(ctx, job, reviews, head, analysis.Decision)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailureSummary, err)
	}

	publishedReview, err := service.github.SubmitReview(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
		githubapp.SubmitReviewRequest{
			CommitID: head,
			Body:     body,
			Event:    analysis.Decision,
			Comments: comments,
		},
	)
	if err != nil {
		return service.failCheck(ctx, job, checkRun.ID, checkFailurePublish, err)
	}
	logger.InfoContext(
		ctx,
		"review published",
		slog.Int64("review_id", publishedReview.ID),
		slog.String("event", string(analysis.Decision)),
		slog.Int("inline_comments", len(comments)),
		slog.Bool("visible_body", true),
	)

	title, summary := successfulCheckOutput(
		service.minimumImportance,
		analysis,
		publishedFindings,
		traceReviews(reviews, service.botLogin),
		traceThreads(threads, service.botLogin),
	)
	if err := service.succeed(ctx, job, checkRun.ID, title, summary); err != nil {
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

func successfulCheckOutput(
	minimumImportance int,
	analysis Analysis,
	publishedFindings []domain.Finding,
	reviews []reviewTrace,
	threads []threadTrace,
) (string, string) {
	title := "Approved"
	if analysis.Decision == domain.ReviewDecisionRequestChanges {
		title = "Changes requested"
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Decision: `%s`\n\n", analysis.Decision)
	fmt.Fprintf(&summary, "- Minimum importance: `%d`\n", minimumImportance)
	fmt.Fprintf(&summary, "- Findings observed: `%d`\n", len(analysis.Observed))
	fmt.Fprintf(&summary, "- Findings eligible: `%d`\n", len(analysis.Anchored))
	fmt.Fprintf(&summary, "- Inline findings published: `%d`\n", len(publishedFindings))
	fmt.Fprintf(&summary, "- Prior bot review IDs: %s\n", formatReviewTraceIDs(reviews))
	fmt.Fprintf(&summary, "- Bot thread IDs: %s\n", formatThreadTraceIDs(threads))
	fmt.Fprintf(&summary, "- Bot threads resolved at analysis: `%d`", countResolvedThreadTraces(threads))
	return title, summary.String()
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
	summary string,
	cause error,
) error {
	logger := gklog.L(ctx)
	if summary == "" {
		summary = checkSummaryFailure
	}
	if checkRunID != 0 {
		completeErr := service.completeCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			checkRunID,
			"failure",
			summary,
			checkFailureDetail(cause),
		)
		if completeErr != nil {
			logger.ErrorContext(ctx, "complete failed check run", slog.String("err", completeErr.Error()))
			return fmt.Errorf("complete check run: %w", completeErr)
		}
	}
	logger.ErrorContext(ctx, "review job failed", slog.String("err", cause.Error()))
	return cause
}

func (service *Service) prepareReviewBody(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
	head domain.HeadSHA,
	decision domain.ReviewDecision,
) (string, error) {
	logger := gklog.L(ctx)
	summaryReview, found := findSummaryReview(reviews, service.botLogin)
	body := RenderBody(head, decision)
	if found {
		if _, err := service.github.UpdateReview(
			ctx,
			job.InstallationID,
			job.Repository,
			job.Number,
			summaryReview.ID,
			body,
		); err != nil {
			logger.ErrorContext(ctx, "update review summary", slog.String("err", err.Error()))
			return "", fmt.Errorf("update review summary: %w", err)
		}
		logger.InfoContext(ctx, "review summary updated", slog.Int64("review_id", summaryReview.ID), slog.Bool("visible", true))
		return marker.Review(head), nil
	}
	return body, nil
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
	err := service.github.CompleteCheckRun(
		completionCtx,
		installationID,
		repository,
		checkRunID,
		conclusion,
		title,
		summary,
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

func selectFindingsForPublication(
	ctx context.Context,
	findings []domain.Finding,
	reviews []githubapp.Review,
	threads []githubapp.ReviewThread,
	botLogin string,
	maximumUnresolvedComments int,
) ([]domain.Finding, error) {
	logger := gklog.L(ctx)
	state := collectPublicationState(reviews, threads, botLogin, maximumUnresolvedComments)
	selected, historySuppressed, capacityDeferred, err := partitionFindings(ctx, findings, state)
	if err != nil {
		logger.ErrorContext(ctx, "identify current findings", slog.String("err", err.Error()))
		return nil, err
	}
	if err := logPublicationSelection(
		ctx,
		findings,
		selected,
		historySuppressed,
		capacityDeferred,
		state,
		maximumUnresolvedComments,
	); err != nil {
		return nil, err
	}
	return selected, nil
}

type publicationState struct {
	history         map[string]struct{}
	historyIDs      []string
	unresolvedCount int
	capacity        int
}

func collectPublicationState(
	reviews []githubapp.Review,
	threads []githubapp.ReviewThread,
	botLogin string,
	maximumUnresolvedComments int,
) publicationState {
	history := make(map[string]struct{})
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		findingMarker, ok := marker.FindFinding(item.Body)
		if ok {
			history[findingMarker.ID] = struct{}{}
		}
	}
	unresolvedCount := 0
	for _, thread := range threads {
		if thread.RootComment.Author != botLogin {
			continue
		}
		if !thread.Resolved {
			unresolvedCount++
		}
		findingMarker, ok := marker.FindFinding(thread.RootComment.Body)
		if ok {
			history[findingMarker.ID] = struct{}{}
		}
	}
	historyIDs := make([]string, 0, len(history))
	for findingID := range history {
		historyIDs = append(historyIDs, findingID)
	}
	sort.Strings(historyIDs)
	capacity := max(maximumUnresolvedComments-unresolvedCount, 0)
	return publicationState{
		history:         history,
		historyIDs:      historyIDs,
		unresolvedCount: unresolvedCount,
		capacity:        capacity,
	}
}

func partitionFindings(
	ctx context.Context,
	findings []domain.Finding,
	state publicationState,
) ([]domain.Finding, []domain.Finding, []domain.Finding, error) {
	logger := gklog.L(ctx)
	candidates := make([]domain.Finding, 0, len(findings))
	historySuppressed := make([]domain.Finding, 0)
	for _, finding := range findings {
		findingID, err := marker.FindingID(finding)
		if err != nil {
			logger.ErrorContext(ctx, "identify finding", slog.String("err", err.Error()))
			return nil, nil, nil, fmt.Errorf("identify finding: %w", err)
		}
		if _, exists := state.history[findingID]; exists {
			historySuppressed = append(historySuppressed, finding)
			continue
		}
		candidates = append(candidates, finding)
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].Importance != candidates[right].Importance {
			return candidates[left].Importance > candidates[right].Importance
		}
		return compareFindings(candidates[left], candidates[right]) < 0
	})
	selectedCount := min(len(candidates), state.capacity)
	selected := append([]domain.Finding{}, candidates[:selectedCount]...)
	capacityDeferred := append([]domain.Finding{}, candidates[selectedCount:]...)
	return selected, historySuppressed, capacityDeferred, nil
}

func logPublicationSelection(
	ctx context.Context,
	current []domain.Finding,
	selected []domain.Finding,
	historySuppressed []domain.Finding,
	capacityDeferred []domain.Finding,
	state publicationState,
	maximumUnresolvedComments int,
) error {
	logger := gklog.L(ctx)
	currentTrace, err := traceFindings(ctx, current)
	if err != nil {
		logger.ErrorContext(ctx, "trace current findings", slog.String("err", err.Error()))
		return fmt.Errorf("trace current findings: %w", err)
	}
	historySuppressedTrace, err := traceFindings(ctx, historySuppressed)
	if err != nil {
		logger.ErrorContext(ctx, "trace history suppressed findings", slog.String("err", err.Error()))
		return fmt.Errorf("trace history suppressed findings: %w", err)
	}
	capacityDeferredTrace, err := traceFindings(ctx, capacityDeferred)
	if err != nil {
		logger.ErrorContext(ctx, "trace capacity deferred findings", slog.String("err", err.Error()))
		return fmt.Errorf("trace capacity deferred findings: %w", err)
	}
	selectedTrace, err := traceFindings(ctx, selected)
	if err != nil {
		logger.ErrorContext(ctx, "trace selected findings", slog.String("err", err.Error()))
		return fmt.Errorf("trace selected findings: %w", err)
	}
	logger.InfoContext(
		ctx,
		"review findings selected",
		slog.Int("configured_cap", maximumUnresolvedComments),
		slog.Int("unresolved_before", state.unresolvedCount),
		slog.Int("capacity", state.capacity),
		slog.Any("history_finding_ids", state.historyIDs),
		slog.Any("current_findings", currentTrace),
		slog.Any("history_suppressed_findings", historySuppressedTrace),
		slog.Any("capacity_deferred_findings", capacityDeferredTrace),
		slog.Any("selected_findings", selectedTrace),
	)
	return nil
}

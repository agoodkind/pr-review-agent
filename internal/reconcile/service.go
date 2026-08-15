// Package reconcile silently resolves earlier bot findings on newer heads.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

// GitHub loads review threads and resolves owned findings.
type GitHub interface {
	ListReviewThreads(context.Context, int64, domain.Repository, int) ([]githubapp.ReviewThread, error)
	GetFile(context.Context, int64, domain.Repository, string, domain.HeadSHA) ([]byte, error)
	Compare(
		context.Context,
		int64,
		domain.Repository,
		domain.HeadSHA,
		domain.HeadSHA,
	) ([]githubapp.ChangedFile, error)
	GetPullRequest(context.Context, int64, domain.Repository, int) (githubapp.PullRequest, error)
	ResolveReviewThread(context.Context, int64, string) error
}

// Model performs one structured reconciliation completion for a batch prompt.
type Model interface {
	Reconcile(context.Context, string) ([]domain.ThreadResolution, error)
}

// Service reconciles unresolved bot findings on one pull request head.
type Service struct {
	github   GitHub
	model    Model
	botLogin string
	logger   *slog.Logger
}

// NewService constructs a reconciliation service.
func NewService(github GitHub, model Model, botLogin string, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		github:   github,
		model:    model,
		botLogin: botLogin,
		logger:   logger,
	}
}

// Reconcile evaluates earlier owned findings, resolves those proven fixed, and returns current thread state.
func (service *Service) Reconcile(ctx context.Context, job domain.ReviewJob) ([]githubapp.ReviewThread, error) {
	logger := service.logger.With(
		slog.String("delivery_id", job.DeliveryID),
		slog.String("repository", job.Repository.Owner+"/"+job.Repository.Name),
		slog.Int("pull_request", job.Number),
	)
	ctx = gklog.WithLogger(ctx, logger)
	pullRequest, err := service.github.GetPullRequest(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
	)
	if err != nil {
		gklog.L(ctx).ErrorContext(ctx, "get pull request for reconciliation", slog.String("err", err.Error()))
		return nil, fmt.Errorf("get pull request: %w", err)
	}
	currentHead := pullRequest.Head
	logger = logger.With(slog.String("head", string(currentHead)))
	ctx = gklog.WithLogger(ctx, logger)

	threads, err := service.github.ListReviewThreads(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
	)
	if err != nil {
		logger.ErrorContext(ctx, "list review threads", slog.String("err", err.Error()))
		return nil, fmt.Errorf("list review threads: %w", err)
	}
	logger.InfoContext(
		ctx,
		"review threads loaded",
		slog.Any("bot_threads", traceBotThreads(threads, service.botLogin)),
	)

	owned := selectOwnedThreads(threads, service.botLogin, currentHead)
	logger.InfoContext(
		ctx,
		"review reconciliation selected",
		slog.Any("thread_node_ids", ownedThreadIDs(owned)),
	)
	if len(owned) == 0 {
		return threads, nil
	}

	prepared := make([]preparedThread, 0, len(owned))
	for _, thread := range owned {
		contextText, ok := service.loadThreadContext(ctx, job, thread, currentHead)
		if !ok {
			continue
		}
		prepared = append(prepared, preparedThread{
			thread: thread,
			text:   formatThreadSection(thread, currentHead, contextText),
		})
	}

	batches := batchPreparedThreads(prepared, config.MaximumPromptBytes)
	logger.InfoContext(
		ctx,
		"review reconciliation analysis started",
		slog.Int("batches", len(batches)),
		slog.Any("thread_node_ids", preparedThreadIDs(prepared)),
	)
	return service.reconcilePrepared(ctx, job, currentHead, threads, batches, logger)
}

func (service *Service) reconcilePrepared(
	ctx context.Context,
	job domain.ReviewJob,
	currentHead domain.HeadSHA,
	threads []githubapp.ReviewThread,
	batches [][]preparedThread,
	logger *slog.Logger,
) ([]githubapp.ReviewThread, error) {
	var reconcileErrors []error
	allResolutions := make([]domain.ThreadResolution, 0)
	resolvedThreads := make([]resolvedThreadTrace, 0)

	for batchIndex, batch := range batches {
		resolutions, err := service.model.Reconcile(ctx, buildBatchPrompt(batch, batchIndex+1, len(batches)))
		if err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("reconcile batch %d/%d: %w", batchIndex+1, len(batches), err))
			continue
		}

		resolutionIndex := indexResolutions(resolutions)
		allResolutions = append(allResolutions, resolutions...)
		for _, item := range batch {
			resolution, ok := resolutionIndex[item.thread.NodeID]
			if !ok {
				continue
			}
			if resolution.Resolution != domain.ResolutionResolved {
				continue
			}

			currentPullRequest, headErr := service.github.GetPullRequest(
				ctx,
				job.InstallationID,
				job.Repository,
				job.Number,
			)
			if headErr != nil {
				reconcileErrors = append(
					reconcileErrors,
					fmt.Errorf("recheck head for thread %s: %w", item.thread.NodeID, headErr),
				)
				continue
			}
			if currentPullRequest.Head != currentHead {
				headChangedErr := errors.New("head changed during reconciliation")
				logger.ErrorContext(
					ctx,
					"head changed during reconciliation",
					slog.String("err", headChangedErr.Error()),
				)
				return threads, errors.Join(append(reconcileErrors, headChangedErr)...)
			}

			if err := service.github.ResolveReviewThread(ctx, job.InstallationID, item.thread.NodeID); err != nil {
				reconcileErrors = append(
					reconcileErrors,
					fmt.Errorf("%s: %w", formatResolveThreadContext(item.thread), err),
				)
				continue
			}
			markThreadResolved(threads, item.thread.NodeID)
			resolvedThreads = append(resolvedThreads, resolvedThreadTrace{
				NodeID:    item.thread.NodeID,
				CommentID: item.thread.RootComment.DatabaseID,
			})
		}
	}
	logger.InfoContext(
		ctx,
		"review reconciliation analysis completed",
		slog.Int("batches", len(batches)),
		slog.Any("resolutions", allResolutions),
		slog.Any("resolved_threads", resolvedThreads),
	)

	if len(reconcileErrors) > 0 {
		logger.ErrorContext(ctx, "reconcile review threads", slog.String("err", errors.Join(reconcileErrors...).Error()))
	}
	return threads, errors.Join(reconcileErrors...)
}

func formatResolveThreadContext(thread domain.OwnedThread) string {
	return fmt.Sprintf(
		"resolve thread %s (outdated=%t, viewer_can_resolve=%t, viewer_can_unresolve=%t)",
		thread.NodeID,
		thread.Outdated,
		thread.ViewerCanResolve,
		thread.ViewerCanUnresolve,
	)
}

func markThreadResolved(threads []githubapp.ReviewThread, nodeID string) {
	for index := range threads {
		if threads[index].NodeID == nodeID {
			threads[index].Resolved = true
			return
		}
	}
}

type preparedThread struct {
	thread domain.OwnedThread
	text   string
}

type botThreadTrace struct {
	NodeID      string `json:"node_id"`
	CommentID   int64  `json:"comment_id"`
	Resolved    bool   `json:"resolved"`
	Outdated    bool   `json:"outdated"`
	FindingID   string `json:"finding_id,omitempty"`
	FindingHead string `json:"finding_head,omitempty"`
	Importance  int    `json:"importance,omitempty"`
}

type resolvedThreadTrace struct {
	NodeID    string `json:"node_id"`
	CommentID int64  `json:"comment_id"`
}

func traceBotThreads(threads []githubapp.ReviewThread, botLogin string) []botThreadTrace {
	traces := make([]botThreadTrace, 0)
	for _, thread := range threads {
		if thread.RootComment.Author != botLogin {
			continue
		}
		trace := botThreadTrace{
			NodeID:      thread.NodeID,
			CommentID:   thread.RootComment.DatabaseID,
			Resolved:    thread.Resolved,
			Outdated:    thread.Outdated,
			FindingID:   "",
			FindingHead: "",
			Importance:  0,
		}
		findingMarker, ok := marker.FindFinding(thread.RootComment.Body)
		if ok {
			trace.FindingID = findingMarker.ID
			trace.FindingHead = string(findingMarker.Head)
			trace.Importance = findingMarker.Importance
		}
		traces = append(traces, trace)
	}
	sort.Slice(traces, func(left, right int) bool {
		return traces[left].NodeID < traces[right].NodeID
	})
	return traces
}

func ownedThreadIDs(threads []domain.OwnedThread) []string {
	ids := make([]string, 0, len(threads))
	for _, thread := range threads {
		ids = append(ids, thread.NodeID)
	}
	return ids
}

func preparedThreadIDs(threads []preparedThread) []string {
	ids := make([]string, 0, len(threads))
	for _, thread := range threads {
		ids = append(ids, thread.thread.NodeID)
	}
	return ids
}

type threadContext struct {
	currentContent string
	compareText    string
}

func emptyThreadContext() threadContext {
	return threadContext{currentContent: "", compareText: ""}
}

func selectOwnedThreads(
	threads []githubapp.ReviewThread,
	botLogin string,
	currentHead domain.HeadSHA,
) []domain.OwnedThread {
	owned := make([]domain.OwnedThread, 0, len(threads))
	for _, thread := range threads {
		if thread.Resolved {
			continue
		}
		if thread.RootComment.Author != botLogin {
			continue
		}
		if strings.TrimSpace(thread.RootComment.Body) == "" {
			continue
		}
		findingHead, finding, err := marker.DecodeFindingBody(thread.RootComment)
		if err != nil {
			continue
		}
		if findingHead == currentHead {
			continue
		}
		owned = append(owned, domain.OwnedThread{
			NodeID:             thread.NodeID,
			Outdated:           thread.Outdated,
			ViewerCanResolve:   thread.ViewerCanResolve,
			ViewerCanUnresolve: thread.ViewerCanUnresolve,
			RootComment:        thread.RootComment,
			Finding:            finding,
			FindingHead:        findingHead,
		})
	}

	sort.Slice(owned, func(left, right int) bool {
		return owned[left].NodeID < owned[right].NodeID
	})
	return owned
}

func (service *Service) loadThreadContext(
	ctx context.Context,
	job domain.ReviewJob,
	thread domain.OwnedThread,
	currentHead domain.HeadSHA,
) (threadContext, bool) {
	normalizedPath, err := marker.NormalizePath(thread.Finding.Path)
	if err != nil {
		return emptyThreadContext(), false
	}

	fileBytes, err := service.github.GetFile(
		ctx,
		job.InstallationID,
		job.Repository,
		normalizedPath,
		currentHead,
	)
	if err != nil {
		return emptyThreadContext(), false
	}

	lines := extractLines(fileBytes, thread.Finding.StartLine, thread.Finding.EndLine)
	if lines == "" {
		return emptyThreadContext(), false
	}

	changedFiles, err := service.github.Compare(
		ctx,
		job.InstallationID,
		job.Repository,
		thread.FindingHead,
		currentHead,
	)
	if err != nil {
		gklog.L(ctx).ErrorContext(ctx, "compare finding head", slog.String("err", err.Error()))
		return emptyThreadContext(), false
	}

	return threadContext{
		currentContent: lines,
		compareText:    formatCompareForPath(changedFiles, normalizedPath),
	}, true
}

func extractLines(content []byte, startLine, endLine int) string {
	if startLine < 1 || endLine < startLine {
		return ""
	}
	lines := strings.Split(string(content), "\n")
	if startLine > len(lines) || endLine > len(lines) {
		return ""
	}
	return strings.Join(lines[startLine-1:endLine], "\n")
}

func formatCompareForPath(changedFiles []githubapp.ChangedFile, path string) string {
	for _, file := range changedFiles {
		if file.Path != path && file.PreviousPath != path {
			continue
		}
		if !file.PatchPresent || strings.TrimSpace(file.Patch) == "" {
			return "No patch available."
		}
		return file.Patch
	}
	return "No changes in this file between finding head and current head."
}

func formatThreadSection(
	thread domain.OwnedThread,
	currentHead domain.HeadSHA,
	contextText threadContext,
) string {
	var builder strings.Builder
	builder.WriteString("Thread node id: ")
	builder.WriteString(thread.NodeID)
	builder.WriteString("\nFinding head: ")
	builder.WriteString(string(thread.FindingHead))
	builder.WriteString("\nCurrent head: ")
	builder.WriteString(string(currentHead))
	builder.WriteString("\nPath: ")
	builder.WriteString(thread.Finding.Path)
	builder.WriteString("\nLines: ")
	fmt.Fprintf(&builder, "%d-%d", thread.Finding.StartLine, thread.Finding.EndLine)
	builder.WriteString("\nTitle: ")
	builder.WriteString(thread.Finding.Title)
	builder.WriteString("\nBody: ")
	builder.WriteString(thread.Finding.Body)
	builder.WriteString("\nImportance: ")
	fmt.Fprintf(&builder, "%d", thread.Finding.Importance)
	builder.WriteString("\n\nCurrent code:\n")
	builder.WriteString(contextText.currentContent)
	builder.WriteString("\n\nDiff from finding head to current head:\n")
	builder.WriteString(contextText.compareText)
	return builder.String()
}

func batchPreparedThreads(threads []preparedThread, maxSize int) [][]preparedThread {
	if len(threads) == 0 {
		return nil
	}
	if maxSize < 1 {
		maxSize = 1
	}

	batches := make([][]preparedThread, 0)
	current := make([]preparedThread, 0)
	currentSize := 0

	flush := func() {
		if len(current) == 0 {
			return
		}
		batchCopy := append([]preparedThread{}, current...)
		batches = append(batches, batchCopy)
		current = make([]preparedThread, 0)
		currentSize = 0
	}

	for _, thread := range threads {
		separator := 0
		if len(current) > 0 {
			separator = 2
		}
		if len(current) > 0 && currentSize+separator+len(thread.text) > maxSize {
			flush()
			separator = 0
		}
		if len(current) == 0 && len(thread.text) > maxSize {
			batches = append(batches, []preparedThread{thread})
			continue
		}
		if len(current) > 0 {
			currentSize += separator
		}
		current = append(current, thread)
		currentSize += len(thread.text)
	}
	flush()
	return batches
}

func buildBatchPrompt(batch []preparedThread, index, total int) string {
	var builder strings.Builder
	builder.WriteString("Reconcile bot threads after a new commit. Batch ")
	fmt.Fprintf(&builder, "%d/%d", index, total)
	builder.WriteString(".\n")
	var body strings.Builder
	for itemIndex, item := range batch {
		if itemIndex > 0 {
			body.WriteString("\n\n")
		}
		body.WriteString(item.text)
	}
	builder.WriteString(review.WrapUntrusted(body.String()))
	return builder.String()
}

func indexResolutions(resolutions []domain.ThreadResolution) map[string]domain.ThreadResolution {
	index := make(map[string]domain.ThreadResolution, len(resolutions))
	for _, resolution := range resolutions {
		index[resolution.ThreadNodeID] = resolution
	}
	return index
}

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
	"goodkind.io/pr-review-agent/internal/diff"
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
	) (githubapp.Comparison, error)
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

var errHeadChanged = errors.New("head changed during reconciliation")

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
	removed := make([]domain.OwnedThread, 0)
	for _, thread := range owned {
		contextText, state := service.loadThreadContext(ctx, job, thread, currentHead)
		switch state {
		case threadContextRemoved:
			removed = append(removed, thread)
			continue
		case threadContextUnavailable:
			continue
		case threadContextPresent:
		}
		prepared = append(prepared, preparedThread{
			thread: thread,
			text:   formatThreadSection(thread, currentHead, contextText, service.botLogin),
		})
	}

	batches := batchPreparedThreads(prepared, config.MaximumPromptBytes)
	logger.InfoContext(
		ctx,
		"review reconciliation analysis started",
		slog.Int("batches", len(batches)),
		slog.Any("thread_node_ids", preparedThreadIDs(prepared)),
	)
	return service.reconcilePrepared(ctx, job, currentHead, threads, removed, batches, logger)
}

func (service *Service) reconcilePrepared(
	ctx context.Context,
	job domain.ReviewJob,
	currentHead domain.HeadSHA,
	threads []githubapp.ReviewThread,
	removed []domain.OwnedThread,
	batches [][]preparedThread,
	logger *slog.Logger,
) ([]githubapp.ReviewThread, error) {
	var reconcileErrors []error
	allResolutions := make([]domain.ThreadResolution, 0)
	resolvedThreads := make([]resolvedThreadTrace, 0)
	for _, thread := range removed {
		resolution := domain.ThreadResolution{
			ThreadNodeID: thread.NodeID,
			Resolution:   domain.ResolutionResolved,
			Reason:       "finding anchor no longer exists",
		}
		allResolutions = append(allResolutions, resolution)
		if err := service.resolveAtHead(ctx, job, currentHead, thread, threads, logger); err != nil {
			reconcileErrors = append(reconcileErrors, err)
			if errors.Is(err, errHeadChanged) {
				return threads, errors.Join(reconcileErrors...)
			}
			continue
		}
		resolvedThreads = append(resolvedThreads, resolvedThreadTrace{
			NodeID:    thread.NodeID,
			CommentID: thread.RootComment.DatabaseID,
		})
	}

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

			if err := service.resolveAtHead(ctx, job, currentHead, item.thread, threads, logger); err != nil {
				reconcileErrors = append(reconcileErrors, err)
				if errors.Is(err, errHeadChanged) {
					return threads, errors.Join(reconcileErrors...)
				}
				continue
			}
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

func (service *Service) resolveAtHead(
	ctx context.Context,
	job domain.ReviewJob,
	currentHead domain.HeadSHA,
	thread domain.OwnedThread,
	threads []githubapp.ReviewThread,
	logger *slog.Logger,
) error {
	currentPullRequest, err := service.github.GetPullRequest(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
	)
	if err != nil {
		return fmt.Errorf("recheck head for thread %s: %w", thread.NodeID, err)
	}
	if currentPullRequest.Head != currentHead {
		logger.ErrorContext(ctx, "head changed during reconciliation", slog.String("err", errHeadChanged.Error()))
		return errHeadChanged
	}
	if err := service.github.ResolveReviewThread(ctx, job.InstallationID, thread.NodeID); err != nil {
		return fmt.Errorf("%s: %w", formatResolveThreadContext(thread), err)
	}
	markThreadResolved(threads, thread.NodeID)
	return nil
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

type threadContextState uint8

const (
	threadContextUnavailable threadContextState = iota
	threadContextPresent
	threadContextRemoved
)

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
			Replies:            thread.Replies,
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
) (threadContext, threadContextState) {
	normalizedPath, err := marker.NormalizePath(thread.Finding.Path)
	if err != nil {
		return emptyThreadContext(), threadContextUnavailable
	}

	comparison, err := service.github.Compare(
		ctx,
		job.InstallationID,
		job.Repository,
		thread.FindingHead,
		currentHead,
	)
	if err != nil {
		gklog.L(ctx).ErrorContext(ctx, "compare finding head", slog.String("err", err.Error()))
		return emptyThreadContext(), threadContextUnavailable
	}
	changedFiles := comparison.Files

	// GitHub compares from where two commits last agreed, so once the finding
	// commit and the current one diverge the patches describe a range the
	// finding's coordinates were never in. There is then no way to say where the
	// anchor went: the recorded line does not identify it here either, and a
	// window drawn on it can show unrelated clean code that reads as the defect
	// being gone. The thread is left unreconciled instead, which keeps it open
	// for a person rather than resolving it on evidence that is not about it.
	if comparison.MergeBase == "" || comparison.MergeBase != thread.FindingHead {
		gklog.L(ctx).WarnContext(
			ctx,
			"thread context unavailable, the comparison is not measured from the finding commit",
			slog.String("thread", thread.NodeID),
			slog.String("merge_base", string(comparison.MergeBase)),
		)
		return emptyThreadContext(), threadContextUnavailable
	}

	currentPath := normalizedPath
	for _, file := range changedFiles {
		if file.Path == normalizedPath && file.Status == "removed" {
			return emptyThreadContext(), threadContextRemoved
		}
		if file.PreviousPath == normalizedPath && file.Path != "" {
			currentPath = file.Path
		}
	}

	fileBytes, err := service.github.GetFile(
		ctx,
		job.InstallationID,
		job.Repository,
		currentPath,
		currentHead,
	)
	if err != nil {
		return emptyThreadContext(), threadContextUnavailable
	}

	startLine, endLine := remapAnchor(comparison, normalizedPath, thread.Finding)
	return threadContext{
		currentContent: extractAnchorWindow(fileBytes, startLine, endLine),
		compareText:    formatCompareForPath(changedFiles, normalizedPath),
	}, threadContextPresent
}

// remapAnchor moves the finding's coordinates from the head it was written
// against to the current head.
//
// A window around the stale line shows the fix only while the shift stays
// inside the radius, and nothing bounds the shift: a commit that inserts twenty
// lines above the anchor moves the code the reconciler needs to see out of view,
// and the model then reads unrelated code and keeps a fixed finding open. The
// compare patch is already loaded here and records every shift exactly, so the
// remapping needs no extra call.
//
// The caller has already established that the patch is measured from the commit
// the finding was written against; a comparison that is not gets no context at
// all rather than a remapped or a stale window. What is left here is the case
// where the patch simply says nothing about this file, or cannot be read, and
// the recorded coordinates are then still the best available.
func remapAnchor(
	comparison githubapp.Comparison,
	normalizedPath string,
	finding domain.Finding,
) (int, int) {
	patch, ok := patchForPath(comparison.Files, normalizedPath)
	if !ok {
		return finding.StartLine, finding.EndLine
	}
	startLine, startMapped := diff.MapLineToNewSide(patch, finding.StartLine)
	endLine, endMapped := diff.MapLineToNewSide(patch, finding.EndLine)
	if !startMapped || !endMapped || endLine < startLine {
		return finding.StartLine, finding.EndLine
	}
	return startLine, endLine
}

// patchForPath returns the compare patch for one file, under the name it had at
// the finding head or the name it carries now.
func patchForPath(changedFiles []githubapp.ChangedFile, normalizedPath string) (string, bool) {
	for _, file := range changedFiles {
		if file.Path != normalizedPath && file.PreviousPath != normalizedPath {
			continue
		}
		if !file.PatchPresent || strings.TrimSpace(file.Patch) == "" {
			return "", false
		}
		return file.Patch, true
	}
	return "", false
}

// anchorWindowRadius is the context shown on each side of the anchor. GitHub
// reports only the original head's coordinates for outdated threads, so later
// commits shift the anchor and an exact-line excerpt would show unrelated code.
const anchorWindowRadius = 15

// extractAnchorWindow returns the lines around the anchor, clamped to the file
// bounds. An anchor past the end of a shortened file still yields the file's
// tail: only a removed file skips the model, never a shorter one.
func extractAnchorWindow(content []byte, startLine, endLine int) string {
	lines := strings.Split(string(content), "\n")
	startLine = min(max(startLine, 1), len(lines))
	endLine = min(max(endLine, startLine), len(lines))
	windowStart := max(startLine-anchorWindowRadius, 1)
	windowEnd := min(endLine+anchorWindowRadius, len(lines))
	return strings.Join(lines[windowStart-1:windowEnd], "\n")
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
	botLogin string,
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
	if len(thread.Replies) > 0 {
		// The replies are not labelled as the author's. Anyone who can comment on
		// a pull request can reply on a thread, so calling them all the author's
		// response would let a passer by, or this service quoting itself, stand as
		// the answer that resolves a finding.
		lines, omitted := review.FormatReplies(thread.Replies, botLogin, review.MaximumReplyBytes)
		builder.WriteString("\n\nReplies on this thread, oldest first. The name before each one is who wrote it")
		if omitted > 0 {
			fmt.Fprintf(&builder, ", and %d older replies are not shown", omitted)
		}
		builder.WriteString(":")
		for _, line := range lines {
			builder.WriteString("\n")
			builder.WriteString(line)
		}
	}
	builder.WriteString("\n\nCurrent code around the anchor (line numbers may have shifted):\n")
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
	builder.WriteString(". Resolve a thread when the current code no longer has its defect. Keep it open only when the defect still exists. Use uncertain only when the supplied code and diff cannot decide. Author replies are context: verify their claims against the supplied code and diff, and resolve when a reply's disproof of the finding holds up there.\n")
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

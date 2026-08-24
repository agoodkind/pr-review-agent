package review

// This file decides which findings a review posts and which decision that
// review can stand behind.
//
// Analysis reports every defect the model found on this head. Publication is
// narrower: a finding an earlier review already carried stays suppressed once
// its thread exists, and the number of open findings is capped. The two answers
// differ, so the review publishes the narrower one.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// standingDecision reports the decision this review can actually stand behind.
//
// The analysis decision covers every finding the model reported on this head.
// Publication is narrower: a finding an earlier review already carried is
// suppressed, and it stays suppressed after its thread is resolved. A review
// that publishes nothing and leaves no unresolved thread therefore has no
// objection a reader can act on, and requesting changes there blocks the pull
// request with nothing to fix and nothing to dismiss but the review itself.
func standingDecision(
	ctx context.Context,
	analysis Analysis,
	published []domain.Finding,
	threads []githubapp.ReviewThread,
	botLogin string,
) domain.ReviewDecision {
	if analysis.Decision != domain.ReviewDecisionRequestChanges {
		return analysis.Decision
	}
	if len(published) > 0 || hasUnresolvedBotThread(threads, botLogin) {
		return domain.ReviewDecisionRequestChanges
	}
	gklog.L(ctx).InfoContext(
		ctx,
		"review decision relaxed to approval",
		slog.String("analysis_decision", string(analysis.Decision)),
		slog.Int("eligible_findings", len(analysis.Anchored)),
		slog.Int("published_findings", 0),
		slog.String("reason", "every eligible finding was already carried by an earlier review and no bot thread is open"),
	)
	return domain.ReviewDecisionApprove
}

// hasUnresolvedBotThread reports whether any earlier bot finding is still open.
// The threads argument is the state after reconciliation, so a thread the
// reconciler just resolved is already marked resolved here.
func hasUnresolvedBotThread(threads []githubapp.ReviewThread, botLogin string) bool {
	for _, thread := range threads {
		if thread.RootComment.Author != botLogin {
			continue
		}
		if !thread.Resolved {
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
	historyIDs        map[string]struct{}
	historyIDList     []string
	historyAnchors    map[string]struct{}
	historyAnchorList []string
	unresolvedCount   int
	capacity          int
}

func collectPublicationState(
	reviews []githubapp.Review,
	threads []githubapp.ReviewThread,
	botLogin string,
	maximumUnresolvedComments int,
) publicationState {
	historyIDs := make(map[string]struct{})
	historyAnchors := make(map[string]struct{})
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		findingMarker, ok := marker.FindFinding(item.Body)
		if ok {
			historyIDs[findingMarker.ID] = struct{}{}
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
			historyIDs[findingMarker.ID] = struct{}{}
			if anchor, valid := findingAnchorKey(
				thread.RootComment.Path,
				thread.RootComment.StartLine,
				thread.RootComment.EndLine,
			); valid {
				historyAnchors[anchor] = struct{}{}
			}
		}
	}
	historyIDList := make([]string, 0, len(historyIDs))
	for findingID := range historyIDs {
		historyIDList = append(historyIDList, findingID)
	}
	sort.Strings(historyIDList)
	historyAnchorList := make([]string, 0, len(historyAnchors))
	for anchor := range historyAnchors {
		historyAnchorList = append(historyAnchorList, anchor)
	}
	sort.Strings(historyAnchorList)
	capacity := max(maximumUnresolvedComments-unresolvedCount, 0)
	return publicationState{
		historyIDs:        historyIDs,
		historyIDList:     historyIDList,
		historyAnchors:    historyAnchors,
		historyAnchorList: historyAnchorList,
		unresolvedCount:   unresolvedCount,
		capacity:          capacity,
	}
}

func findingAnchorKey(pathValue string, startLine int, endLine int) (string, bool) {
	normalizedPath, err := marker.NormalizePath(pathValue)
	if err != nil || startLine < 1 || endLine < startLine {
		return "", false
	}
	return fmt.Sprintf("%s:%d", normalizedPath, endLine), true
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
		anchor, anchorValid := findingAnchorKey(finding.Path, finding.StartLine, finding.EndLine)
		_, foundByID := state.historyIDs[findingID]
		_, foundByAnchor := state.historyAnchors[anchor]
		if foundByID || anchorValid && foundByAnchor {
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
		slog.Any("history_finding_ids", state.historyIDList),
		slog.Any("history_finding_anchors", state.historyAnchorList),
		slog.Any("current_findings", currentTrace),
		slog.Any("history_suppressed_findings", historySuppressedTrace),
		slog.Any("capacity_deferred_findings", capacityDeferredTrace),
		slog.Any("selected_findings", selectedTrace),
	)
	return nil
}

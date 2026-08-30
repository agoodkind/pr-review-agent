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

// logPublishedFindings records what the review found against what reached the
// pull request, so a reader can tell a suppressed finding from a lost one.
func logPublishedFindings(
	ctx context.Context,
	eligible []domain.Finding,
	published []domain.Finding,
	posted int,
	failed int,
) {
	logger := gklog.L(ctx)
	eligibleTrace, err := traceFindings(ctx, eligible)
	if err != nil {
		logger.ErrorContext(ctx, "trace eligible findings", slog.String("err", err.Error()))
		return
	}
	publishedTrace, err := traceFindings(ctx, published)
	if err != nil {
		logger.ErrorContext(ctx, "trace published findings", slog.String("err", err.Error()))
		return
	}
	logger.InfoContext(
		ctx,
		"review findings published",
		slog.Int("eligible", len(eligible)),
		slog.Int("published", len(published)),
		slog.Int("comments_posted", posted),
		slog.Int("comments_rejected", failed),
		slog.Any("eligible_findings", eligibleTrace),
		slog.Any("published_findings", publishedTrace),
	)
}

// publicationState decides which findings a run may publish.
//
// It starts from what earlier reviews and threads already carried, and it grows
// as findings are confirmed delivered, so a finding published from one chunk
// suppresses the same finding reported by a later chunk. The caller serializes
// access, because chunks run concurrently.
//
// One slot out of the configured cap is never spent immediately; it is held
// back for the run's single tail slot, arbitrated in stream.go once every
// chunk has answered. Without that, whichever chunk happened to answer first
// could spend the last slot on a low importance finding and leave no room for
// a more severe one a later chunk reports.
type publicationState struct {
	historyIDs     map[string]struct{}
	historyAnchors map[string]struct{}
	capacity       int
	hasTailSlot    bool
}

// findingKeys are the two identities a finding is suppressed by. Both cost a
// hash and a path normalization, so they are computed once per finding.
type findingKeys struct {
	id          string
	anchor      string
	anchorValid bool
}

func keysFor(finding domain.Finding) findingKeys {
	keys := findingKeys{id: "", anchor: "", anchorValid: false}
	if findingID, err := marker.FindingID(finding); err == nil {
		keys.id = findingID
	}
	keys.anchor, keys.anchorValid = findingAnchorKey(finding.Path, finding.StartLine, finding.EndLine)
	return keys
}

// candidate is one finding with the identities used to suppress it, carried
// together so ordering cannot separate them.
type candidate struct {
	finding domain.Finding
	keys    findingKeys
}

// suppressed reports whether an earlier review or an earlier chunk already
// carried this finding.
func (state *publicationState) suppressed(keys findingKeys) bool {
	if keys.id != "" {
		if _, seen := state.historyIDs[keys.id]; seen {
			return true
		}
	}
	if !keys.anchorValid {
		return false
	}
	_, seen := state.historyAnchors[keys.anchor]
	return seen
}

// remember records a finding as delivered, so no later chunk repeats it and no
// future run reports it again. Called only once a comment has actually posted;
// a reservation that never delivers must never reach here.
func (state *publicationState) remember(keys findingKeys) {
	if keys.id != "" {
		state.historyIDs[keys.id] = struct{}{}
	}
	if keys.anchorValid {
		state.historyAnchors[keys.anchor] = struct{}{}
	}
}

// outranks reports whether one candidate should be shown ahead of another when
// capacity forces a choice between them.
func outranks(left candidate, right candidate) bool {
	if left.finding.Importance != right.finding.Importance {
		return left.finding.Importance > right.finding.Importance
	}
	return compareFindings(left.finding, right.finding) < 0
}

// sortByImportanceThenPosition orders candidates worst first, so a scarce
// capacity shows the worst defects rather than the fastest chunk's.
func sortByImportanceThenPosition(candidates []candidate) {
	sort.SliceStable(candidates, func(left, right int) bool {
		return outranks(candidates[left], candidates[right])
	})
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
	totalCapacity := max(maximumUnresolvedComments-unresolvedCount, 0)
	return publicationState{
		historyIDs:     historyIDs,
		historyAnchors: historyAnchors,
		capacity:       max(totalCapacity-1, 0),
		hasTailSlot:    totalCapacity >= 1,
	}
}

func findingAnchorKey(pathValue string, startLine int, endLine int) (string, bool) {
	normalizedPath, err := marker.NormalizePath(pathValue)
	if err != nil || startLine < 1 || endLine < startLine {
		return "", false
	}
	return fmt.Sprintf("%s:%d", normalizedPath, endLine), true
}

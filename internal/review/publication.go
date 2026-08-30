package review

// This file decides which findings a review posts and which decision that
// review can stand behind.
//
// A run reports every defect it found on this head, with one exception: a
// finding an earlier review already carried stays suppressed once its thread
// exists, so the same defect is raised once and not once per push.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// reviewerDecision is the whole verdict policy. A reviewer blocks while
// something they raised is open, and approves when nothing is. Anything more
// clever than this is how stale blocks happened: a decision derived from what
// one run happened to find outlives the run, while the threads a reader can
// actually act on say something else entirely.
//
// Both inputs are read after this run's findings are published, so the threads
// include the ones this run just opened, and a head that was not fully
// reviewed is never approved on the strength of a partial read.
func reviewerDecision(
	threads []githubapp.ReviewThread,
	botLogin string,
	headFullyReviewed bool,
) domain.ReviewDecision {
	if hasUnresolvedBotThread(threads, botLogin) {
		return domain.ReviewDecisionRequestChanges
	}
	if !headFullyReviewed {
		return domain.ReviewDecisionRequestChanges
	}
	return domain.ReviewDecisionApprove
}

// hasUnresolvedBotThread reports whether any finding of the service's own is
// still open.
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

// blockingReasons states everything holding a requesting-changes verdict: one
// line per open thread of the service's own, and one line when the head was
// not fully reviewed.
//
// A run that finds nothing new still blocks while an earlier thread is
// unresolved, and with nothing said about it the review reads as a silent
// repeat. One live pull request carried three blocking reviews, two of them
// empty, and no reader could tell that a single unresolved thread was the
// whole cause.
func blockingReasons(
	threads []githubapp.ReviewThread,
	botLogin string,
	ref domain.PullRequestRef,
	headFullyReviewed bool,
) []string {
	reasons := make([]string, 0)
	for _, thread := range threads {
		if thread.RootComment.Author != botLogin || thread.Resolved {
			continue
		}
		reasons = append(reasons, describeOpenThread(thread, ref))
	}
	if !headFullyReviewed {
		reasons = append(reasons, unreviewedHeadReason)
	}
	return reasons
}

// unreviewedHeadReason explains a block that no open thread accounts for: the
// run could not read the whole head, or could not post what it found there.
const unreviewedHeadReason = "This head was not fully reviewed, so nothing here can approve it yet. " +
	"The next push reviews what this run could not."

// describeOpenThread names one open thread the way a reader can act on it: the
// place in the code it objects to, linked to the comment itself.
func describeOpenThread(thread githubapp.ReviewThread, ref domain.PullRequestRef) string {
	label := strings.TrimSpace(thread.RootComment.Path)
	if label == "" {
		label = thread.NodeID
	} else if thread.RootComment.EndLine > 0 {
		label = fmt.Sprintf("%s:%d", label, thread.RootComment.EndLine)
	}
	if thread.RootComment.DatabaseID == 0 {
		return label
	}
	return fmt.Sprintf("[%s](%s)", label, threadCommentURL(thread, ref))
}

// threadCommentURL builds the permalink GitHub gives one review comment.
func threadCommentURL(thread githubapp.ReviewThread, ref domain.PullRequestRef) string {
	return fmt.Sprintf(
		"https://github.com/%s/%s/pull/%d#discussion_r%d",
		ref.Repository.Owner,
		ref.Repository.Name,
		ref.Number,
		thread.RootComment.DatabaseID,
	)
}

// GitHub reports review states, not the events that produced them: the event
// REQUEST_CHANGES reads back as CHANGES_REQUESTED and APPROVE as APPROVED.
const (
	reviewStateApproved         = "APPROVED"
	reviewStateChangesRequested = "CHANGES_REQUESTED"
)

// latestBotVerdictReview returns the newest review of the service's own that
// still carries a verdict; COMMENTED and DISMISSED reviews decide nothing.
func latestBotVerdictReview(reviews []githubapp.Review, botLogin string) (githubapp.Review, bool) {
	latest := githubapp.Review{ID: 0, CommitID: "", Author: "", Body: "", State: ""}
	found := false
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		if item.State != reviewStateApproved && item.State != reviewStateChangesRequested {
			continue
		}
		latest = item
		found = true
	}
	return latest, found
}

// reviewStateFor is the state GitHub reports once a decision is submitted.
func reviewStateFor(decision domain.ReviewDecision) string {
	switch decision {
	case domain.ReviewDecisionApprove:
		return reviewStateApproved
	case domain.ReviewDecisionRequestChanges:
		return reviewStateChangesRequested
	case domain.ReviewDecisionComment:
		return "COMMENTED"
	default:
		return ""
	}
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
// as this run carries findings of its own, so a defect two chunks both report
// is posted once. Chunks are reviewed one at a time, so no locking is needed.
type publicationState struct {
	historyIDs     map[string]struct{}
	historyAnchors map[string]struct{}
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

// remember records a finding as carried by this run, so no later chunk repeats
// it. It is called before the comment is attempted, because two chunks
// reporting the same defect must produce one comment whether or not the first
// attempt succeeded.
func (state *publicationState) remember(keys findingKeys) {
	if keys.id != "" {
		state.historyIDs[keys.id] = struct{}{}
	}
	if keys.anchorValid {
		state.historyAnchors[keys.anchor] = struct{}{}
	}
}

// collectPublicationState reads what the pull request already carries, which is
// the only reason a run withholds a finding it stands behind.
func collectPublicationState(
	reviews []githubapp.Review,
	threads []githubapp.ReviewThread,
	botLogin string,
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
	for _, thread := range threads {
		if thread.RootComment.Author != botLogin {
			continue
		}
		findingMarker, ok := marker.FindFinding(thread.RootComment.Body)
		if !ok {
			continue
		}
		historyIDs[findingMarker.ID] = struct{}{}
		if anchor, valid := findingAnchorKey(
			thread.RootComment.Path,
			thread.RootComment.StartLine,
			thread.RootComment.EndLine,
		); valid {
			historyAnchors[anchor] = struct{}{}
		}
	}
	return publicationState{
		historyIDs:     historyIDs,
		historyAnchors: historyAnchors,
	}
}

func findingAnchorKey(pathValue string, startLine int, endLine int) (string, bool) {
	normalizedPath, err := marker.NormalizePath(pathValue)
	if err != nil || startLine < 1 || endLine < startLine {
		return "", false
	}
	return fmt.Sprintf("%s:%d", normalizedPath, endLine), true
}

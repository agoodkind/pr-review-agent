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
	// reviewStateDismissed is a verdict somebody withdrew. It is not a verdict
	// and it does not restore the one before it.
	reviewStateDismissed = "DISMISSED"
)

// latestBotVerdictReview returns the newest review of the service's own that
// still carries a verdict for this head; COMMENTED and DISMISSED reviews decide
// nothing.
//
// The head is part of the test, not context. A pull request force pushed back to
// a commit it already carried has verdicts from more than one head in one list,
// and a scan that took the newest of them judged this head by what some other
// head concluded. The commit a review names is what settles which head it spoke
// for.
//
// The review marker is deliberately not also required. Every verdict body
// carries one, so it would exclude nothing a matching commit does not already
// exclude, and a verdict whose marker never reached the review list is exactly
// the case the durable state path exists to refresh.
func latestBotVerdictReview(
	reviews []githubapp.Review,
	botLogin string,
	head domain.HeadSHA,
) (githubapp.Review, bool) {
	latest := githubapp.Review{ID: 0, CommitID: "", Author: "", Body: "", State: ""}
	found := false
	for _, item := range reviews {
		if item.Author != botLogin || item.CommitID != head {
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

// latestBotVerdictState is the state GitHub currently shows for this service on
// the pull request: its newest review carrying a verdict, whatever head that
// review named. It is the empty string when the service has submitted none.
//
// Which head a review named decides what that run concluded, and decides nothing
// about what the pull request shows now. A force push back to a commit that was
// already reviewed leaves a newer review from the head in between, and GitHub
// keeps counting that newer one. Comparing a recomputed decision against the
// matching head's older review found the two equal, submitted nothing, and left
// the newer verdict standing over thread state that no longer supported it.
func latestBotVerdictState(reviews []githubapp.Review, botLogin string) string {
	state := ""
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		// A dismissal withdraws the verdict rather than restoring the one before
		// it. Passing over it would leave the older state standing, and a refresh
		// that then found that state equal to the decision it recomputed would
		// submit nothing, leaving the pull request carrying no verdict at all.
		if item.State == reviewStateDismissed {
			state = ""
			continue
		}
		if item.State != reviewStateApproved && item.State != reviewStateChangesRequested {
			continue
		}
		state = item.State
	}
	return state
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
// is posted once. Every read and write of it happens under the pass lock,
// because chunks answer several at a time.
type publicationState struct {
	historyIDs     map[string]struct{}
	historyAnchors map[string]struct{}
	// carried is what this run has already published, compared through the one
	// shared comparison rather than on identity alone.
	//
	// Identity is a hash of the path and the normalized title, so a second chunk
	// rewording the same defect produced a different identity and posted a
	// second comment. The claim key and the anchor range survive that rewording,
	// so what a chunk carried is recognized when a later chunk restates it.
	carried *claimMemory
}

// findingKeys are the identities a finding is suppressed by. Each costs a hash
// and a path normalization, so they are computed once per finding.
type findingKeys struct {
	id          string
	anchor      string
	anchorValid bool
	// duplicate is the claim key and anchor range this run compares a candidate
	// against what it has already carried.
	duplicate duplicateKeys
}

func keysFor(finding domain.Finding) findingKeys {
	keys := findingKeys{
		id:          "",
		anchor:      "",
		anchorValid: false,
		duplicate:   candidateKeys(finding),
	}
	if findingID, err := marker.FindingID(finding); err == nil {
		keys.id = findingID
	}
	keys.anchor, keys.anchorValid = findingAnchorKey(finding.Path, finding.StartLine, finding.EndLine)
	return keys
}

// suppressed reports whether an earlier review or an earlier chunk already
// carried this finding, and names the layer and the evidence that decided it.
func (state *publicationState) suppressed(keys findingKeys) (string, duplicateMatch, bool) {
	if keys.id != "" {
		if _, seen := state.historyIDs[keys.id]; seen {
			return layerPriorReviews, duplicateMatch{
				Sense:   senseIdentity,
				Detail:  shortClaimKey(keys.id),
				Matched: "a finding an earlier review already carried",
			}, true
		}
	}
	if keys.anchorValid {
		if _, seen := state.historyAnchors[keys.anchor]; seen {
			return layerPriorReviews, duplicateMatch{
				Sense:   senseAnchorLine,
				Detail:  keys.anchor,
				Matched: "a thread anchored on that line",
			}, true
		}
	}
	if match, seen := state.carried.match(keys.duplicate); seen {
		return layerAcrossChunks, match, true
	}
	return "", noMatch(), false
}

// remember records a finding as carried by this run, so no later chunk repeats
// it. It is called before the comment is attempted, because two chunks
// reporting the same defect must produce one comment whether or not the first
// attempt succeeded.
func (state *publicationState) remember(keys findingKeys, label string) {
	if keys.id != "" {
		state.historyIDs[keys.id] = struct{}{}
	}
	if keys.anchorValid {
		state.historyAnchors[keys.anchor] = struct{}{}
	}
	state.carried.remember(keys.duplicate, label)
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
		carried:        newClaimMemory(),
	}
}

func findingAnchorKey(pathValue string, startLine int, endLine int) (string, bool) {
	normalizedPath, err := marker.NormalizePath(pathValue)
	if err != nil || startLine < 1 || endLine < startLine {
		return "", false
	}
	return fmt.Sprintf("%s:%d", normalizedPath, endLine), true
}

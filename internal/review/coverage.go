package review

// This file is what a run leaves behind when part of the head is beyond what
// this service can read at all.
//
// A shortfall has two kinds and they need opposite endings. A chunk whose model
// call or comment post failed is temporary: it stays pending, the baseline
// holds, and the next push really does finish it. A hunk larger than one model
// request, a binary file, and a patch GitHub will not supply come back
// identically on every later run, so pending them promises a push that cannot
// deliver, and blocking with a verdict leaves a person a review to dismiss over
// code this service was never going to read.
//
// So a structural shortfall ends here instead. It submits no verdict, holds the
// merge gate with an action_required check, names every piece nobody read, and
// leaves the durable baseline where it was so those pieces stay in every later
// delta rather than vanishing behind an advanced checkpoint. The findings the
// readable chunks produced are already on the pull request, because they are
// real whatever else went unread.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode/utf8"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/runlog"
)

// unreadHunk names one piece of the head nobody read, in the terms a reader can
// go and look at: the file, the hunk inside it, and why it was not read.
type unreadHunk struct {
	Path   string
	Header string
	Reason string
}

// structuralShortfall is everything one delta holds that no later run can read.
type structuralShortfall struct {
	Hunks []unreadHunk
}

// present reports whether this delta holds anything unreadable at all.
func (shortfall structuralShortfall) present() bool {
	return len(shortfall.Hunks) > 0
}

// paths names the files involved, for the log line.
func (shortfall structuralShortfall) paths() []string {
	paths := make([]string, 0, len(shortfall.Hunks))
	for _, hunk := range shortfall.Hunks {
		paths = append(paths, hunk.Path)
	}
	return paths
}

// Reasons a piece of the head went unread, in this service's own words. Each
// one is published on the pull request, so none of them quotes anything this
// service did not write.
const (
	oversizedHunkReason   = "larger than one model request allows"
	binaryFileReason      = "a binary file, which carries no reviewable patch"
	patchAbsentReason     = "GitHub supplied no patch for this file"
	patchUnreadableReason = "the patch GitHub supplied could not be read whole"
	contentMissingReason  = "GitHub will not serve this file's content at this commit"
	// truncatedAnswerReason names a hunk the model began answering and never
	// finished. A chunk whose answer runs past the completion budget is normally
	// halved and each half asked separately, so this is reached only by a chunk
	// already down to one hunk, which is the smallest unit a chunk is cut into.
	truncatedAnswerReason = "the model's answer ran past its completion budget, and a hunk cannot be split further"
)

// sortedUnreadHunks orders unread hunks by path and then by hunk. Chunks answer
// concurrently, so without this the same run could name the same hunks in a
// different order on the check run and in the comment.
func sortedUnreadHunks(hunks []unreadHunk) []unreadHunk {
	ordered := append([]unreadHunk{}, hunks...)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].Path != ordered[right].Path {
			return ordered[left].Path < ordered[right].Path
		}
		return ordered[left].Header < ordered[right].Header
	})
	return ordered
}

// classifyStructuralShortfall names every piece of this delta that will go
// unread on every later run as surely as it did on this one.
//
// It reads the delta rather than the pass. What one pass managed to do says
// nothing about whether a hunk fits in a model request, and a run that re-reads
// nothing because an earlier run already read every chunk must still reach the
// same verdict about the same oversized hunk.
func classifyStructuralShortfall(work deltaWork) structuralShortfall {
	hunks := make([]unreadHunk, 0)
	for _, file := range work.Files {
		if !file.Gap.Recurs() {
			continue
		}
		hunks = append(hunks, unreadHunk{
			Path:   file.Path,
			Header: "",
			Reason: fileGapReason(file.Gap),
		})
	}
	for _, chunk := range work.Chunks {
		for _, piece := range chunk.Pieces {
			if !piece.Oversized {
				continue
			}
			hunks = append(hunks, unreadHunk{
				Path:   piece.Path,
				Header: piece.Header,
				Reason: oversizedHunkReason,
			})
		}
	}
	return structuralShortfall{Hunks: hunks}
}

// fileGapReason states why a whole file went unread.
func fileGapReason(gap diff.CoverageGap) string {
	switch gap {
	case diff.CoverageGapBinary:
		return binaryFileReason
	case diff.CoverageGapPatchAbsent:
		return patchAbsentReason
	case diff.CoverageGapPatchUnreadable:
		return patchUnreadableReason
	case diff.CoverageGapContentMissing:
		return contentMissingReason
	case diff.CoverageGapNone, diff.CoverageGapContentUnavailable:
		return ""
	default:
		return ""
	}
}

// concludeStructurallyIncomplete ends a pass over a head this service cannot
// read whole.
//
// No review object is touched. A verdict here would be a judgment on code
// nobody read, and the blocking one this used to publish left a person holding
// a requested-changes review that no push could clear and only a manual
// dismissal could remove. The check holds the gate instead, and says what has
// to happen for the pull request to move.
func (service *Service) concludeStructurallyIncomplete(
	ctx context.Context,
	job domain.ReviewJob,
	checkRun githubapp.CheckRun,
	state marker.State,
	shortfall structuralShortfall,
	summary Summary,
	progress *reviewProgress,
) error {
	logger := gklog.L(ctx)
	notice := structuralShortfallNotice(summary.Head, shortfall, len(state.Pending))
	if err := service.upsertSummaryComment(ctx, job, summaryCommentContent{
		Prose: RenderUnreadableBody(summary, notice),
		State: state,
	}); err != nil {
		return service.failCheck(
			ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureSummary, err,
		)
	}
	if err := service.completeCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		checkRun.ID,
		checkConclusionDeclined,
		unreadableCheckTitle(len(shortfall.Hunks)),
		notice+"\n\n"+RenderDetails(summary),
	); err != nil {
		return err
	}
	logger.InfoContext(
		ctx,
		"review head holds changes this service cannot read",
		slog.Int("unread_hunks", len(shortfall.Hunks)),
		slog.Any("unread_paths", shortfall.paths()),
		slog.Int("pending", len(state.Pending)),
		slog.Int64("check_run_id", checkRun.ID),
	)
	return nil
}

// unreadableCheckTitle is the one line a reader sees in the checks list.
//
// It says the count and no more. Every path is in the summary below it, where
// length is not a constraint and a long path cannot crowd out the sentence.
func unreadableCheckTitle(count int) string {
	return hunkCount(count) + " cannot be reviewed by this service"
}

// structuralShortfallNotice is the prose the check run and the visible comment
// both carry: what went unread, why, and what a person has to do about it.
//
// It never says the next push covers this. The whole point of the class is that
// the next push finds the same hunk and falls short the same way, so promising
// otherwise is how a pull request sat blocked waiting for a run that was never
// going to happen.
func structuralShortfallNotice(
	head domain.HeadSHA,
	shortfall structuralShortfall,
	pending int,
) string {
	count := len(shortfall.Hunks)
	return strings.Join([]string{
		fmt.Sprintf(
			"`%s` carries %s this service cannot read, and a later run reaches the same limit on %s.",
			shortHead(head),
			hunkCount(count),
			hunkPronoun(count),
		),
		renderUnreadHunks(shortfall.Hunks),
		"Read " + hunkPronoun(count) + " yourself, or split the pull request so every change is " +
			"small enough to review.",
		remainingWorkSentence(pending),
	}, "\n\n")
}

// remainingWorkSentence says what this run still owes beyond the hunks above.
//
// It used to claim that everything else on the head was reviewed whatever else
// had happened, which is false the moment a chunk is left pending: those chunks
// were not read either, and a reader told the rest was covered has no reason to
// wait for the run that covers them.
func remainingWorkSentence(pending int) string {
	if pending == 0 {
		return "Everything else on this head was reviewed, and anything found there is already inline."
	}
	return fmt.Sprintf(
		"%s went unread as well and a later run retries %s, so the rest of this head is not covered yet. "+
			"Anything found so far is already inline.",
		chunkCount(pending),
		chunkPronoun(pending),
	)
}

// maximumListedUnreadHunks bounds the list a notice prints.
//
// A check run output is capped by size, and one line per unread hunk over a
// pull request touching hundreds of files runs past that cap, which leaves the
// check unfinished and reports nothing at all. The count in the sentence above
// stays exact; only the list is cut.
const maximumListedUnreadHunks = 40

// maximumUnreadHunkLabelBytes bounds one line of that list, because a single
// repository path can be long enough to crowd out the rest on its own.
const maximumUnreadHunkLabelBytes = 220

// renderUnreadHunks lists each unread hunk as inert code text.
func renderUnreadHunks(hunks []unreadHunk) string {
	listed := hunks
	omitted := 0
	if len(listed) > maximumListedUnreadHunks {
		omitted = len(listed) - maximumListedUnreadHunks
		listed = listed[:maximumListedUnreadHunks]
	}
	lines := make([]string, 0, len(listed)+4)
	lines = append(lines, "Not read:", "```")
	for _, hunk := range listed {
		lines = append(lines, describeUnreadHunk(hunk))
	}
	lines = append(lines, "```")
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("and %d more not listed here.", omitted))
	}
	return strings.Join(lines, "\n")
}

// describeUnreadHunk names one unread piece the way a reader can go and find it.
func describeUnreadHunk(hunk unreadHunk) string {
	label := escapeUnreadHunkText(hunk.Path)
	if hunk.Header != "" {
		label += " " + escapeUnreadHunkText(hunk.Header)
	}
	if len(label) > maximumUnreadHunkLabelBytes {
		label = truncateUTF8(label, maximumUnreadHunkLabelBytes) + "..."
	}
	if hunk.Reason == "" {
		return label
	}
	return label + " (" + hunk.Reason + ")"
}

// truncateUTF8 keeps the byte limit without splitting a rune.
func truncateUTF8(text string, maximumBytes int) string {
	end := min(len(text), maximumBytes)
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end]
}

// escapeUnreadHunkText prevents repository text from closing the code fence.
func escapeUnreadHunkText(text string) string {
	withoutLines := runlog.EscapeLineBreaks(text)
	return strings.ReplaceAll(withoutLines, "`", string(rune(0x2CB)))
}

// hunkCount names a number of unread hunks without the plural mismatch a bare
// count leaves in a sentence a person reads.
func hunkCount(count int) string {
	if count == 1 {
		return "1 hunk"
	}
	return fmt.Sprintf("%d hunks", count)
}

// hunkPronoun matches hunkCount, so the sentence around it agrees.
func hunkPronoun(count int) string {
	if count == 1 {
		return "it"
	}
	return "them"
}

// RenderUnreadableBody renders the visible comment for a head this service
// cannot read whole.
//
// It carries no review marker, for the same reason the progress body carries
// none: that marker means this head was reviewed, and this comment says the
// opposite.
func RenderUnreadableBody(summary Summary, notice string) string {
	return strings.Join([]string{"## Review", notice, RenderDetails(summary)}, "\n\n")
}

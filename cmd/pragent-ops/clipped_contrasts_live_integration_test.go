//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

func TestLiveClippedContrastsRemainBlockingAtEachLocation(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_CLIPPED_CONTRASTS_TEST") != "1" {
		t.Skip("The deployed clipped-contrast regression is not enabled.")
	}
	fixture := clippedLiveSetup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()
	originalFile := fixture.file(t, ctx)
	originalContent := clippedLiveDecode(t, originalFile)
	bad := clippedLivePassage(t, "clipped-contrast-bad.txt")
	fixed := clippedLivePassage(t, "clipped-contrast-corrected.txt")
	if bad == fixed || strings.Contains(originalContent, bad) {
		t.Fatal("The fixture requires distinct passages and an original file without the violation.")
	}
	if strings.Count(fixed, "\n") > strings.Count(bad, "\n") {
		t.Fatal("The replacement passage must fit within the original passage lines.")
	}
	baseline := fixture.await(t, ctx, "APPROVED", nil, 0)
	for _, thread := range baseline.Threads {
		if thread.RootComment.Author == fixture.bot && !thread.Resolved {
			t.Fatal("The dedicated fixture must not contain unresolved bot findings.")
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cleanupCancel()
		fixture.write(t, cleanupCtx, originalContent, "Restore prose fixture content")
		fixture.await(t, cleanupCtx, "APPROVED", nil, 0)
		current := fixture.pullRequest(t, cleanupCtx)
		if current.Title != fixture.original.Title || current.Body != fixture.original.Body || current.Draft != fixture.original.Draft || current.State != fixture.original.State {
			t.Error("The fixture metadata changed during the live regression.")
		}
		if clippedLiveDecode(t, fixture.file(t, cleanupCtx)) != originalContent {
			t.Error("The original fixture file did not survive cleanup.")
		}
	})
	first := clippedLiveDocument(originalContent, bad, bad, "")
	fixture.write(t, ctx, first, "Add repeated contrast passages to prose fixture")
	blocked := fixture.await(t, ctx, "CHANGES_REQUESTED", nil, 2)
	threads := clippedLiveOccurrences(t, blocked, fixture.bot, fixture.filePath, fixture.head)
	if len(threads) != 2 || threads[0].RootComment.StartLine == threads[1].RootComment.StartLine {
		t.Fatal("The deployed review did not identify both separate occurrences.")
	}
	clippedLiveAssertAnchors(t, threads, first, bad)
	firstID, secondID := threads[0].NodeID, threads[1].NodeID
	if threads[0].RootComment.StartLine > threads[1].RootComment.StartLine {
		firstID, secondID = secondID, firstID
	}
	fixture.write(t, ctx, clippedLiveDocument(originalContent, clippedLivePad(fixed, bad), bad, ""), "Correct the first contrast passage in prose fixture")
	fixture.await(t, ctx, "CHANGES_REQUESTED", map[string]bool{firstID: true, secondID: false}, 1)
	clean := clippedLiveDocument(originalContent, clippedLivePad(fixed, bad), clippedLivePad(fixed, bad), "")
	fixture.write(t, ctx, clean, "Correct the second contrast passage in prose fixture")
	fixture.await(t, ctx, "APPROVED", map[string]bool{firstID: true, secondID: true}, 0)
	third := clippedLiveDocument(originalContent, clippedLivePad(fixed, bad), clippedLivePad(fixed, bad), bad)
	fixture.write(t, ctx, third, "Add the contrast passage at another fixture location")
	reintroduced := fixture.await(t, ctx, "CHANGES_REQUESTED", map[string]bool{firstID: true, secondID: true}, 1)
	newThreads := clippedLiveOccurrences(t, reintroduced, fixture.bot, fixture.filePath, fixture.head)
	if len(newThreads) != 1 || newThreads[0].NodeID == firstID || newThreads[0].NodeID == secondID {
		t.Fatal("The new occurrence did not create its own blocking finding.")
	}
	clippedLiveAssertAnchors(t, newThreads, third, bad)
	t.Logf("The deployed service identified both occurrences, resolved each corrected thread, approved the corrected document, and blocked a new occurrence on %s#%d.", fixture.opts.repository, fixture.opts.pullRequest)
}

func (fixture *clippedLiveFixture) await(t *testing.T, parent context.Context, decision string, resolved map[string]bool, openCount int) reviewBodySnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 6*time.Minute)
	defer cancel()
	last := "The deployed review has not completed the current commit."
	for ctx.Err() == nil {
		if fixture.pullRequest(t, ctx).Head.SHA != fixture.head {
			t.Fatal("The fixture head changed while waiting for the deployed review.")
		}
		snapshot, err := captureReviewBodies(ctx, fixture.client, fixture.opts.installationID, fixture.repo, fixture.opts.pullRequest)
		if err != nil {
			t.Fatal("The deployed fixture review could not be read.")
		}
		if validateReviewBodySummary(snapshot, fixture.bot) == nil {
			actual := githubapp.LatestDecisionReview(snapshot.Reviews, fixture.bot).State
			open := 0
			seen := make(map[string]bool)
			matched := true
			for _, thread := range snapshot.Threads {
				if thread.RootComment.Author != fixture.bot {
					continue
				}
				if !thread.Resolved {
					open++
				}
				if want, exists := resolved[thread.NodeID]; exists {
					seen[thread.NodeID] = true
					matched = matched && want == thread.Resolved
				}
			}
			next := fmt.Sprintf("The latest decision is %s with %d unresolved findings. The expected thread resolutions match: %t.", actual, open, matched && len(seen) == len(resolved))
			if next != last {
				t.Log(next)
				last = next
			}
			if actual == decision && open == openCount && matched && len(seen) == len(resolved) {
				return snapshot
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	t.Fatalf("The deployed review did not complete with decision %s and %d unresolved findings at %s within six minutes. %s", decision, openCount, fixture.head, last)
	return reviewBodySnapshot{}
}

func clippedLiveOccurrences(t *testing.T, snapshot reviewBodySnapshot, bot, filePath, head string) []githubapp.ReviewThread {
	t.Helper()
	var result []githubapp.ReviewThread
	for _, thread := range snapshot.Threads {
		if thread.RootComment.Author != bot || thread.RootComment.Path != filePath || thread.Resolved {
			continue
		}
		finding, valid := marker.FindFinding(thread.RootComment.Body)
		if !valid || string(finding.Head) != head || finding.Importance != 10 {
			t.Fatal("The fixture occurrence must have a current-commit finding with importance ten.")
		}
		result = append(result, thread)
	}
	return result
}

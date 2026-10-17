//go:build integration

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

func TestLiveClippedContrastsRemainBlockingInDescription(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_CLIPPED_CONTRASTS_TEST") != "1" {
		t.Skip("The deployed clipped-contrast regression is not enabled.")
	}
	fixture := clippedLiveSetup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()
	bad := clippedLivePassage(t, "clipped-contrast-bad.txt")
	fixed := clippedLivePassage(t, "clipped-contrast-corrected.txt")
	if strings.Contains(fixture.original.Body, bad) || strings.Count(fixed, "\n") > strings.Count(bad, "\n") || bad == fixed {
		t.Fatal("The description fixture requires clean original prose and a distinct replacement within the original line count.")
	}
	fixture.awaitMetadata(t, ctx, fixture.original.Body, "APPROVED", 0)
	expectedBody := fixture.original.Body
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cleanupCancel()
		fixture.editBody(t, cleanupCtx, expectedBody, fixture.original.Body)
		fixture.awaitMetadata(t, cleanupCtx, fixture.original.Body, "APPROVED", 0)
	})
	duplicated := clippedLiveDocument(fixture.original.Body, bad, bad, "")
	fixture.editBody(t, ctx, expectedBody, duplicated)
	expectedBody = duplicated
	findings := fixture.awaitMetadata(t, ctx, expectedBody, "CHANGES_REQUESTED", 2)
	clippedLiveAssertMetadataAnchors(t, findings, expectedBody, bad)
	firstFixed := clippedLiveDocument(fixture.original.Body, clippedLivePad(fixed, bad), bad, "")
	fixture.editBody(t, ctx, expectedBody, firstFixed)
	expectedBody = firstFixed
	findings = fixture.awaitMetadata(t, ctx, expectedBody, "CHANGES_REQUESTED", 1)
	clippedLiveAssertMetadataAnchors(t, findings, expectedBody, bad)
	fixture.editBody(t, ctx, expectedBody, fixture.original.Body)
	expectedBody = fixture.original.Body
	fixture.awaitMetadata(t, ctx, expectedBody, "APPROVED", 0)
	t.Logf("The deployed description review retained both occurrences, blocked the remaining occurrence after one correction, and approved the restored description on %s#%d.", fixture.opts.repository, fixture.opts.pullRequest)
}

func (fixture *clippedLiveFixture) editBody(t *testing.T, ctx context.Context, expected, replacement string) {
	t.Helper()
	current := fixture.pullRequest(t, ctx)
	if current.Head.SHA != fixture.head || current.Head.Ref != fixture.original.Head.Ref || current.Title != fixture.original.Title || current.Body != expected {
		t.Fatal("The fixture changed concurrently. The description edit will preserve the external change.")
	}
	if expected == replacement {
		return
	}
	var updated clippedLivePR
	clippedLiveAPI(t, ctx, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", fixture.opts.repository, fixture.opts.pullRequest), struct {
		Body string `json:"body"`
	}{replacement}, &updated)
	if updated.Head.SHA != fixture.head || updated.Head.Ref != fixture.original.Head.Ref || updated.Title != fixture.original.Title || updated.Body != replacement {
		t.Fatal("The updated description did not retain the expected head, branch, and title.")
	}
}

func (fixture *clippedLiveFixture) awaitMetadata(t *testing.T, parent context.Context, body, decision string, count int) []domain.Finding {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 6*time.Minute)
	defer cancel()
	last := "The deployed service has not completed the edited description."
	for ctx.Err() == nil {
		current := fixture.pullRequest(t, ctx)
		if current.Head.SHA != fixture.head || current.Title != fixture.original.Title || current.Body != body {
			t.Fatal("The fixture changed while waiting for the description review.")
		}
		snapshot := liveReviewBodyCapture(t, ctx, fixture.client, fixture.opts, fixture.repo)
		findings, revisionCurrent := clippedLiveMetadata(snapshot, fixture.bot)
		actual := githubapp.LatestDecisionReview(snapshot.Reviews, fixture.bot).State
		completed := validateReviewBodySummary(snapshot, fixture.bot) == nil
		next := fmt.Sprintf("The latest decision is %s. The summary completion is %t. The metadata revision match is %t. The summary contains %d metadata findings.", actual, completed, revisionCurrent, len(findings))
		if next != last {
			t.Log(next)
			last = next
		}
		if completed && revisionCurrent && actual == decision && len(findings) == count {
			return findings
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	t.Fatalf("The description review did not produce %s with %d metadata findings within six minutes. %s", decision, count, last)
	return nil
}

func clippedLiveMetadata(snapshot reviewBodySnapshot, bot string) ([]domain.Finding, bool) {
	const prefix = "<!-- pr-review-agent:metadata:v1 "
	for _, comment := range snapshot.Comments {
		if comment.Author != bot || !marker.HasState(comment.Body) {
			continue
		}
		index := strings.LastIndex(comment.Body, prefix)
		if index < 0 {
			continue
		}
		payload, _, found := strings.Cut(comment.Body[index+len(prefix):], " -->")
		decoded, err := base64.RawURLEncoding.DecodeString(payload)
		var record struct {
			Head     domain.HeadSHA   `json:"head"`
			Revision string           `json:"revision"`
			Findings []domain.Finding `json:"findings"`
		}
		if found && err == nil && json.Unmarshal(decoded, &record) == nil {
			return record.Findings, record.Head == snapshot.PullRequest.Head && record.Revision == diff.PullRequestRevision(snapshot.PullRequest)
		}
	}
	return nil, false
}

func clippedLiveAssertMetadataAnchors(t *testing.T, findings []domain.Finding, document, passage string) {
	t.Helper()
	var anchors []githubapp.ReviewThread
	for _, finding := range findings {
		if finding.EffectiveSurface() != domain.FindingPullRequestDescription || finding.RuleID != "clipped_contrasts" || finding.Importance != 10 {
			t.Fatal("The description findings must apply the clipped_contrasts rule with importance ten.")
		}
		anchors = append(anchors, githubapp.ReviewThread{RootComment: domain.ReviewComment{StartLine: finding.StartLine, EndLine: finding.EndLine}})
	}
	clippedLiveAssertAnchors(t, anchors, document, passage)
}

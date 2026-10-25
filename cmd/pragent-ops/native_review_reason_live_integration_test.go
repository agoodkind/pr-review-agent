//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestLiveNativeReviewReasonsFollowUnresolvedFindings(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_NATIVE_REVIEW_REASON_TEST") != "1" {
		t.Skip("The native review explanation regression is not enabled.")
	}
	opts := liveReviewBodyOptions(t)
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := liveFailureConfig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	directory, err := os.MkdirTemp("", "pragent-native-review-reason-")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(directory, "service.ndjson")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := logFile.Close(); err != nil {
			t.Error(err)
		}
	})
	service, client := liveFailureService(cfg, slog.New(slog.NewJSONHandler(logFile, nil)))
	original := liveReviewBodyCapture(t, ctx, client, opts, repo)
	if original.PullRequest.State != "open" || !original.PullRequest.Draft || !strings.Contains(strings.ToLower(original.PullRequest.Title), "fixture") || len(original.Threads) != 0 {
		t.Fatal("The explanation regression requires an open draft fixture without inline threads.")
	}
	if err := writeReviewBodyBackup(filepath.Join(directory, "before.json"), original); err != nil {
		t.Fatal(err)
	}
	t.Logf("The private explanation evidence directory is %s.", directory)
	reference := domain.PullRequestRef{Repository: repo, InstallationID: opts.installationID, Number: opts.pullRequest, Head: original.PullRequest.Head}
	comment := liveApprovalSummary(t, ctx, client, reference, cfg.GitHubBotLogin)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if _, err := client.UpdateIssueComment(cleanupCtx, opts.installationID, repo, comment.ID, comment.Body); err != nil {
			t.Error(err)
		}
	})
	const path = "docs/pr-agent-native-review-link-fixture.md"
	var summary review.Summary
	summary.Head, summary.Decision = reference.Head, domain.ReviewDecisionApprove
	summary.CoverageComplete, summary.OmissionsAccepted = true, true
	summary.MinimumImportance = cfg.MinimumImportance
	summary.MetadataRevision = diff.PullRequestRevision(original.PullRequest)
	summary.FileAssessmentMaximumBytes = cfg.ReviewPolicy.FileAssessmentMaximumBytes()
	summary.FileAssessments = []domain.FileAssessment{{
		Path: path, Identity: string(reference.Head), ContentKind: reviewrules.ContentSource,
		ContentReviewRequirement: domain.ContentReviewRequired, Action: domain.FileActionReview, Reviewed: true,
		Reason: "The fixture content received a complete review.", Evidence: []string{"file:" + string(reference.Head)},
	}}
	var assessmentRecord string
	for _, line := range strings.Split(review.RenderVerdictBody(summary), "\n") {
		if strings.HasPrefix(line, "<!-- pr-review-agent:file-assessments:v1 ") {
			assessmentRecord = line
		}
	}
	if assessmentRecord == "" {
		t.Fatal("The fixture must persist its completed file assessment before native publication.")
	}
	var state marker.State
	state.LastReviewed, state.Status, state.RunID = reference.Head, marker.StateDone, "native-reason-seed-"+rand.Text()
	if _, err := client.UpdateIssueComment(ctx, opts.installationID, repo, comment.ID, review.RenderBody(summary)+"\n"+marker.EncodeState(state)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SubmitReview(ctx, opts.installationID, repo, opts.pullRequest, githubapp.SubmitReviewRequest{CommitID: reference.Head, Event: domain.ReviewDecisionApprove}); err != nil {
		t.Fatal(err)
	}
	const markedReason = "The fixture does not validate the requested operation."
	const legacyReason = "The fixture does not restrict deletion targets."
	finding := domain.Finding{Path: path, StartLine: 3, EndLine: 3, Title: "Validate the fixture operation", Body: markedReason + " Validate the operation before processing it.", Importance: 8}
	inline, err := review.RenderInline(reference.Head, []domain.Finding{finding})
	if err != nil {
		t.Fatal(err)
	}
	legacy := githubapp.InlineComment{Path: path, Line: 1, Side: "RIGHT", Body: "### Restrict the fixture deletion scope\n\n" + legacyReason + " Keep this additional explanation in the inline finding."}
	expectedBodies := map[string]bool{inline[0].Body: true, legacy.Body: true}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		current, err := client.ListReviewThreads(cleanupCtx, opts.installationID, repo, opts.pullRequest)
		if err != nil {
			t.Error(err)
			return
		}
		for _, item := range current {
			if item.RootComment.Author != cfg.GitHubBotLogin || item.RootComment.Path != path || !expectedBodies[item.RootComment.Body] {
				t.Error("An unexpected fixture finding requires inspection before cleanup.")
				continue
			}
			endpoint := fmt.Sprintf("repos/%s/pulls/comments/%d", opts.repository, item.RootComment.DatabaseID)
			if output, err := exec.CommandContext(cleanupCtx, "gh", "api", "--method", "DELETE", endpoint).CombinedOutput(); err != nil {
				t.Errorf("The owned fixture finding could not be removed: %v: %.512s", err, output)
			}
		}
	})
	for _, item := range []githubapp.InlineComment{inline[0], legacy} {
		if err := client.CreateReviewComment(ctx, opts.installationID, repo, opts.pullRequest, reference.Head, item); err != nil {
			t.Fatal(err)
		}
	}
	threads, err := client.ListReviewThreads(ctx, opts.installationID, repo, opts.pullRequest)
	if err != nil || len(threads) != 2 {
		t.Fatal("GitHub must retain both explicitly created fixture findings.")
	}
	before := liveApprovalReviews(t, ctx, client, reference)
	if _, _, err := liveApprovalRun(t, ctx, service, reference, nil); err != nil {
		t.Fatal(err)
	}
	after := liveApprovalReviews(t, ctx, client, reference)
	if len(after) != len(before)+1 {
		t.Fatal("The first unresolved finding must produce exactly one native rejection.")
	}
	rejection := after[len(after)-1]
	if rejection.State != "CHANGES_REQUESTED" || strings.TrimSpace(strings.Split(rejection.Body, "<!--")[0]) != markedReason {
		t.Fatal("The native rejection must use one sentence from the highest-importance unresolved finding.")
	}
	if !strings.Contains(rejection.Body, assessmentRecord) {
		t.Fatal("The native rejection must preserve the completed file assessment in its recovery checkpoint.")
	}
	withheld := summary
	withheld.Decision, withheld.ApprovalWithheld = domain.ReviewDecisionComment, true
	if _, err := client.SubmitReview(ctx, opts.installationID, repo, opts.pullRequest, githubapp.SubmitReviewRequest{CommitID: reference.Head, Event: domain.ReviewDecisionComment, Body: review.RenderVerdictBody(withheld)}); err != nil {
		t.Fatal(err)
	}
	for _, item := range threads {
		if item.RootComment.Body == inline[0].Body {
			if err := client.ResolveReviewThread(ctx, opts.installationID, item.NodeID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := liveApprovalRun(t, ctx, service, reference, nil); err != nil {
		t.Fatal(err)
	}
	retained := liveApprovalReviews(t, ctx, client, reference)
	index := slices.IndexFunc(retained, func(item githubapp.Review) bool { return item.ID == rejection.ID })
	if index < 0 || len(retained) != len(after)+1 {
		t.Fatal("The later comment must preserve the standing rejection without creating another verdict.")
	}
	last := retained[index]
	if last.State != "CHANGES_REQUESTED" || strings.TrimSpace(strings.Split(last.Body, "<!--")[0]) != legacyReason {
		t.Fatal("The remaining legacy finding must update the existing native explanation without another review.")
	}
	if !strings.Contains(last.Body, assessmentRecord) {
		t.Fatal("Updating the native explanation must preserve the completed file assessment.")
	}
	for _, item := range threads {
		if err := client.ResolveReviewThread(ctx, opts.installationID, item.NodeID); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := liveApprovalRun(t, ctx, service, reference, nil); err != nil {
		t.Fatal(err)
	}
	approved := liveApprovalReviews(t, ctx, client, reference)
	if last := approved[len(approved)-1]; last.State != "APPROVED" || last.Body != "" {
		t.Fatal("Resolving both findings must approve without a native approval body.")
	}
	if err := logFile.Sync(); err != nil {
		t.Fatal(err)
	}
	liveApprovalAssertNoProviderRequests(t, logPath)
	t.Logf("Native review %d updated its explanation without a duplicate review or provider request.", rejection.ID)
}

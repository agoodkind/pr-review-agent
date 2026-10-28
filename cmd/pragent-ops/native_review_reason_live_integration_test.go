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
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
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
	if !original.PullRequest.Draft || !strings.Contains(strings.ToLower(original.PullRequest.Title), "fixture") || len(original.Threads) != 0 {
		t.Fatal("The explanation regression requires a draft fixture without inline threads.")
	}
	if err := writeReviewBodyBackup(filepath.Join(directory, "before.json"), original); err != nil {
		t.Fatal(err)
	}
	if original.PullRequest.State == "closed" {
		if output, err := exec.CommandContext(ctx, "gh", "pr", "reopen", strconv.Itoa(opts.pullRequest), "--repo", opts.repository).CombinedOutput(); err != nil {
			t.Fatalf("Reopen the dedicated draft fixture: %v: %.512s", err, output)
		}
		t.Cleanup(func() {
			recovery, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			if output, err := exec.CommandContext(recovery, "gh", "pr", "close", strconv.Itoa(opts.pullRequest), "--repo", opts.repository).CombinedOutput(); err != nil {
				t.Errorf("Close the dedicated fixture: %v: %.512s", err, output)
			}
		})
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
		if _, err := client.SubmitReview(cleanupCtx, opts.installationID, repo, opts.pullRequest, githubapp.SubmitReviewRequest{CommitID: reference.Head, Event: domain.ReviewDecisionApprove}); err != nil {
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
	before := liveApprovalReviews(t, ctx, client, reference)
	rejection, err := client.SubmitReview(ctx, opts.installationID, repo, opts.pullRequest, githubapp.SubmitReviewRequest{
		CommitID: reference.Head, Event: domain.ReviewDecisionRequestChanges, Comments: []githubapp.InlineComment{inline[0]},
	})
	if err != nil || rejection.State != "CHANGES_REQUESTED" || rejection.Body != "" {
		t.Fatal("GitHub must accept the first inline finding without a review body.")
	}
	if err := client.CreateReviewComment(ctx, opts.installationID, repo, opts.pullRequest, reference.Head, legacy); err != nil {
		t.Fatal(err)
	}
	threads, err := client.ListReviewThreads(ctx, opts.installationID, repo, opts.pullRequest)
	if err != nil || len(threads) != 2 {
		t.Fatal("GitHub must retain both explicitly created fixture findings.")
	}
	if _, _, err := liveApprovalRun(t, ctx, service, reference, nil); err != nil {
		t.Fatal(err)
	}
	after := liveApprovalReviews(t, ctx, client, reference)
	if len(after) != len(before)+2 {
		t.Fatal("The public refresh must preserve the inline rejection and the independent later inline review.")
	}
	index := slices.IndexFunc(after, func(item githubapp.Review) bool { return item.ID == rejection.ID })
	if index < 0 || after[index] != rejection {
		t.Fatal("The public refresh must preserve the empty native body and its original review identity.")
	}
	retainedSummary := liveApprovalSummary(t, ctx, client, reference, cfg.GitHubBotLogin)
	if retainedSummary.ID != comment.ID || !strings.Contains(retainedSummary.Body, assessmentRecord) {
		t.Fatal("The single editable summary must preserve the completed file assessment.")
	}
	dismissed, err := client.DismissReview(ctx, opts.installationID, repo, opts.pullRequest, rejection.ID, "The operator withdrew this fixture rejection.")
	if err != nil || dismissed.State != "DISMISSED" {
		t.Fatalf("Withdraw the open fixture rejection before testing refresh: %v", err)
	}
	if _, _, err := liveApprovalRun(t, ctx, service, reference, nil); err != nil {
		t.Fatal(err)
	}
	withdrawn := liveApprovalReviews(t, ctx, client, reference)
	index = slices.IndexFunc(withdrawn, func(item githubapp.Review) bool { return item.ID == rejection.ID })
	if len(withdrawn) != len(after) || index < 0 || withdrawn[index].State != "DISMISSED" || withdrawn[index].Body != "" {
		t.Fatal("The public refresh restated the manually withdrawn rejection.")
	}
	currentThreads, err := client.ListReviewThreads(ctx, opts.installationID, repo, opts.pullRequest)
	if err != nil || len(currentThreads) != 2 || currentThreads[0].Resolved || currentThreads[1].Resolved {
		t.Fatal("Manual withdrawal must preserve both unresolved inline findings.")
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
	index = slices.IndexFunc(retained, func(item githubapp.Review) bool { return item.ID == rejection.ID })
	if index < 0 || len(retained) != len(after)+1 {
		t.Fatal("The later comment must preserve the standing rejection without creating another verdict.")
	}
	last := retained[index]
	if last.State != "DISMISSED" || last.Body != "" {
		t.Fatal("The remaining finding must preserve the manual withdrawal without adding a review body.")
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
	t.Logf("Native review %d retained its empty body across public refreshes without another rejection or provider request.", rejection.ID)
}

func TestLiveEmptyRejectionDoesNotProveCompletedReview(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_NATIVE_REVIEW_REASON_TEST") != "1" {
		t.Skip("The native review regression requires an explicit fixture.")
	}
	opts := liveReviewBodyOptions(t)
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := liveFailureConfig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	directory, err := os.MkdirTemp("", "pragent-empty-rejection-coverage-")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(directory, "service.ndjson")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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
	if !original.PullRequest.Draft || !strings.Contains(strings.ToLower(original.PullRequest.Title), "fixture") || len(original.Threads) != 0 {
		t.Fatal("Select a draft fixture without inline threads.")
	}
	if err := writeReviewBodyBackup(filepath.Join(directory, "before.json"), original); err != nil {
		t.Fatal(err)
	}
	reference := domain.PullRequestRef{Repository: repo, InstallationID: opts.installationID, Number: opts.pullRequest, Head: original.PullRequest.Head}
	comment := liveApprovalSummary(t, ctx, client, reference, cfg.GitHubBotLogin)
	var ownedCommentID int64
	t.Cleanup(func() {
		recovery, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if ownedCommentID > 0 {
			endpoint := fmt.Sprintf("repos/%s/pulls/comments/%d", opts.repository, ownedCommentID)
			if output, err := exec.CommandContext(recovery, "gh", "api", "--method", "DELETE", endpoint).CombinedOutput(); err != nil {
				t.Errorf("Delete the fixture finding: %v: %.512s", err, output)
			}
		}
		if _, err := client.UpdateIssueComment(recovery, opts.installationID, repo, comment.ID, comment.Body); err != nil {
			t.Error(err)
		}
		if _, err := client.SubmitReview(recovery, opts.installationID, repo, opts.pullRequest, githubapp.SubmitReviewRequest{CommitID: reference.Head, Event: domain.ReviewDecisionApprove}); err != nil {
			t.Error(err)
		}
	})
	var summary review.Summary
	summary.Head, summary.Decision = reference.Head, domain.ReviewDecisionRequestChanges
	summary.MinimumImportance = cfg.MinimumImportance
	summary.MetadataRevision = diff.PullRequestRevision(original.PullRequest)
	var state marker.State
	state.RunID, state.Status = "empty-rejection-coverage-"+rand.Text(), marker.StateFailed
	state.Pending = []string{"aaaaaaaaaaaa"}
	var lines []string
	for _, line := range strings.Split(review.RenderBody(summary), "\n") {
		if !strings.HasPrefix(line, "<!-- pr-review-agent:review:v1 ") {
			lines = append(lines, line)
		}
	}
	seeded := strings.Join(lines, "\n") + "\n" + marker.EncodeState(state)
	if _, err := client.UpdateIssueComment(ctx, opts.installationID, repo, comment.ID, seeded); err != nil {
		t.Fatal(err)
	}
	const path = "docs/pr-agent-native-review-link-fixture.md"
	finding := domain.Finding{Path: path, StartLine: 3, EndLine: 3, Title: "Verify the fixture operation", Body: "The fixture exercises an incomplete review with one published finding.", Importance: 8}
	inline, err := review.RenderInline(reference.Head, []domain.Finding{finding})
	if err != nil {
		t.Fatal(err)
	}
	rejection, err := client.SubmitReview(ctx, opts.installationID, repo, opts.pullRequest, githubapp.SubmitReviewRequest{CommitID: reference.Head, Event: domain.ReviewDecisionRequestChanges, Comments: inline})
	if err != nil || rejection.Body != "" {
		t.Fatal("Publish the fixture finding without a native review body.")
	}
	threads, err := client.ListReviewThreads(ctx, opts.installationID, repo, opts.pullRequest)
	if err != nil || len(threads) != 1 {
		t.Fatal("Read the single fixture finding.")
	}
	ownedCommentID = threads[0].RootComment.DatabaseID
	if err := client.ResolveReviewThread(ctx, opts.installationID, threads[0].NodeID); err != nil {
		t.Fatal(err)
	}
	job, outcome, err := liveApprovalRun(t, ctx, service, reference, nil)
	if err != nil || outcome.Disposition != domain.AssessmentDeclined || outcome.CoverageComplete {
		t.Fatalf("The missing completion record must withhold approval: %v", err)
	}
	after := liveApprovalReviews(t, ctx, client, reference)
	liveApprovalAssertProtectedReviews(t, original.Reviews, after)
	if len(after) != len(original.Reviews)+1 {
		t.Fatal("The incomplete refresh published another native review.")
	}
	current := liveApprovalSummary(t, ctx, client, reference, cfg.GitHubBotLogin)
	stored, valid := marker.DecodeState(current.Body)
	if current.ID != comment.ID || !valid || stored.LastReviewed != "" || len(stored.Pending) != 1 || stored.Pending[0] != state.Pending[0] {
		t.Fatal("The incomplete refresh discarded the pending checkpoint.")
	}
	check, found, err := client.FindCheckRunByExternalID(ctx, opts.installationID, repo, reference.Head, config.ReviewCheckName, job.DeliveryID)
	if err != nil || !found || check.Conclusion != "action_required" {
		t.Fatal("The incomplete refresh must preserve a nonpassing check.")
	}
	if err := logFile.Sync(); err != nil {
		t.Fatal(err)
	}
	liveApprovalAssertNoProviderRequests(t, logPath)
	t.Logf("Empty rejection %d did not approve the unresolved checkpoint. Private evidence: %s", rejection.ID, directory)
}

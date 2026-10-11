//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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
)

type liveApprovalMetadata struct {
	Head     domain.HeadSHA `json:"head"`
	Revision string         `json:"revision"`
}

func TestLiveApprovalPublicationUsesEmptyBodiesAndDeliveryPolicy(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_APPROVAL_PUBLICATION_TEST") != "1" {
		t.Skip("The live approval publication regression is not enabled.")
	}
	reference, olderHead := liveApprovalTarget(t)
	cfg, invalidCredential := liveFailureConfig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	logPath := filepath.Join(t.TempDir(), "service.ndjson")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal("The private service log could not be created.")
	}
	t.Cleanup(func() {
		if err := logFile.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error("The private service log could not be closed.")
		}
	})
	service, github := liveFailureService(cfg, slog.New(slog.NewJSONHandler(logFile, nil)))
	service.SetApproveEachCommit(false)
	original, err := github.GetPullRequest(ctx, reference.InstallationID, reference.Repository, reference.Number)
	title := strings.ToLower(original.Title)
	if err != nil || original.State != "open" || original.Draft || !original.EligibilityKnown || !original.MetadataKnown || (!strings.Contains(title, "test") && !strings.Contains(title, "fixture")) {
		t.Fatal("The selected fixture must be an open, non-draft test pull request with validated metadata.")
	}
	reference.Head = original.Head
	commits, err := github.ListPullRequestCommits(ctx, reference.InstallationID, reference.Repository, reference.Number)
	olderIndex := slices.IndexFunc(commits, func(commit githubapp.PullRequestCommit) bool { return commit.SHA == olderHead })
	currentIndex := slices.IndexFunc(commits, func(commit githubapp.PullRequestCommit) bool { return commit.SHA == original.Head })
	if err != nil || olderIndex < 0 || currentIndex <= olderIndex {
		t.Fatal("The selected older commit must precede the current commit in the test pull request.")
	}
	threads, err := github.ListReviewThreads(ctx, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil || slices.ContainsFunc(threads, func(thread githubapp.ReviewThread) bool {
		return thread.RootComment.Author == cfg.GitHubBotLogin && !thread.Resolved
	}) {
		t.Fatal("The test pull request must have no unresolved bot review threads.")
	}
	before := liveApprovalReviews(t, ctx, github, reference)
	standing := githubapp.LatestDecisionReview(before, cfg.GitHubBotLogin)
	if standing.State == "APPROVED" || standing.State == "CHANGES_REQUESTED" {
		t.Fatal("The test pull request must have no active bot approval or requested-changes decision.")
	}
	if slices.ContainsFunc(before, func(item githubapp.Review) bool {
		return item.Author == cfg.GitHubBotLogin && item.CommitID == reference.Head && item.State != "DISMISSED"
	}) {
		t.Fatal("The test pull request must have no existing bot verdict at the current commit.")
	}
	originalSummary := liveApprovalSummary(t, ctx, github, reference, cfg.GitHubBotLogin)
	createdReviews := make(map[int64]struct{})
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		restoredSummary, err := github.UpdateIssueComment(cleanupCtx, reference.InstallationID, reference.Repository, originalSummary.ID, originalSummary.Body)
		if err != nil || restoredSummary != originalSummary {
			t.Error("The original test summary could not be restored.")
		}
		currentReviews, err := github.ListReviews(cleanupCtx, reference.InstallationID, reference.Repository, reference.Number)
		if err != nil {
			t.Error("The test reviews could not be read for cleanup.")
			return
		}
		for _, item := range currentReviews {
			if _, owned := createdReviews[item.ID]; !owned || item.Author != cfg.GitHubBotLogin || item.State == "DISMISSED" {
				continue
			}
			dismissed, err := github.DismissReview(cleanupCtx, reference.InstallationID, reference.Repository, reference.Number, item.ID, "The live approval regression has finished.")
			if err != nil || dismissed.ID != item.ID || dismissed.Author != cfg.GitHubBotLogin || dismissed.State != "DISMISSED" {
				t.Errorf("Test review %d could not be dismissed.", item.ID)
			}
		}
		restoredReviews, err := github.ListReviews(cleanupCtx, reference.InstallationID, reference.Repository, reference.Number)
		if err != nil {
			t.Error("The restored review states could not be verified.")
			return
		}
		liveApprovalAssertProtectedReviews(t, before, restoredReviews)
	})
	seeded, err := github.SubmitReview(ctx, reference.InstallationID, reference.Repository, reference.Number, githubapp.SubmitReviewRequest{
		CommitID: olderHead, Body: "", Event: domain.ReviewDecisionApprove, Comments: nil,
	})
	if err != nil {
		t.Fatal("The prior test approval could not be submitted.")
	}
	createdReviews[seeded.ID] = struct{}{}
	if seeded.Author != cfg.GitHubBotLogin || seeded.CommitID != olderHead || seeded.State != "APPROVED" {
		t.Fatal("The prior test approval does not match the selected bot and older commit.")
	}
	var summary review.Summary
	summary.Head = reference.Head
	summary.Decision = domain.ReviewDecisionApprove
	summary.CoverageComplete = true
	summary.MinimumImportance = cfg.MinimumImportance
	summary.MetadataRevision = diff.PullRequestRevision(original)
	var state marker.State
	state.LastReviewed, state.RunID, state.Status = reference.Head, "live-approval-seed-"+rand.Text(), marker.StateDone
	seedBody := review.RenderBody(summary) + "\n\n" + marker.EncodeState(state)
	if _, err := github.UpdateIssueComment(ctx, reference.InstallationID, reference.Repository, originalSummary.ID, seedBody); err != nil {
		t.Fatal("The completed test summary could not be seeded.")
	}
	baseline := liveApprovalReviews(t, ctx, github, reference)
	for _, stage := range []struct {
		label         string
		servicePolicy bool
		delivery      *bool
		wantNew       bool
	}{
		{label: "default policy", servicePolicy: false, delivery: nil, wantNew: false},
		{label: "explicit false overrides true", servicePolicy: true, delivery: liveApprovalBool(false), wantNew: false},
		{label: "explicit true overrides false", servicePolicy: false, delivery: liveApprovalBool(true), wantNew: true},
		{label: "repeated current commit", servicePolicy: false, delivery: liveApprovalBool(true), wantNew: false},
	} {
		service.SetApproveEachCommit(stage.servicePolicy)
		job, outcome, runErr := liveApprovalRun(t, ctx, service, reference, stage.delivery)
		after := liveApprovalReviews(t, ctx, github, reference)
		var added []githubapp.Review
		for _, item := range after {
			if slices.ContainsFunc(baseline, func(old githubapp.Review) bool { return old.ID == item.ID }) {
				continue
			}
			if item.Author == cfg.GitHubBotLogin {
				createdReviews[item.ID] = struct{}{}
			}
			added = append(added, item)
		}
		if runErr != nil || outcome.Disposition != domain.AssessmentCompleted || outcome.Nonce == "" {
			t.Fatal("The production service did not complete the test verdict refresh.")
		}
		check, found, err := github.FindCheckRunByExternalID(ctx, job.InstallationID, job.Repository, job.Head, config.ReviewCheckName, job.DeliveryID)
		if err != nil || !found || check.ID != job.CheckRunID || check.Status != "completed" || check.Conclusion != "success" || check.Outcome.Nonce != outcome.Nonce {
			t.Fatal("GitHub did not retain the completed verdict refresh and its durable outcome.")
		}
		liveApprovalAssertProtectedReviews(t, baseline, after)
		if stage.wantNew {
			if len(added) != 1 || added[0].Author != cfg.GitHubBotLogin || added[0].State != "APPROVED" || added[0].CommitID != reference.Head || added[0].Body != "" {
				t.Fatalf("The %s stage did not publish exactly one approval with an empty body at the current commit.", stage.label)
			}
		} else if len(added) != 0 {
			t.Fatalf("The %s stage published a duplicate review.", stage.label)
		}
		currentSummary := liveApprovalSummary(t, ctx, github, reference, cfg.GitHubBotLogin)
		liveApprovalAssertSummary(t, currentSummary, originalSummary.ID, reference.Head, summary.MetadataRevision, invalidCredential)
		current, err := github.GetPullRequest(ctx, reference.InstallationID, reference.Repository, reference.Number)
		if err != nil || !reflect.DeepEqual(current, original) {
			t.Fatal("The test pull request metadata changed during the live regression.")
		}
		t.Logf("The %s stage completed check %d with %d new reviews and summary %d.", stage.label, job.CheckRunID, len(added), currentSummary.ID)
		baseline = after
	}
	if err := logFile.Sync(); err != nil {
		t.Fatal("The private service log could not be synchronized.")
	}
	liveApprovalAssertNoProviderRequests(t, logPath)
}

func liveApprovalTarget(t *testing.T) (domain.PullRequestRef, domain.HeadSHA) {
	t.Helper()
	parts := strings.Split(os.Getenv("PR_AGENT_LIVE_REPOSITORY"), "/")
	number, numberErr := strconv.Atoi(os.Getenv("PR_AGENT_LIVE_PULL_REQUEST"))
	installation, installationErr := strconv.ParseInt(os.Getenv("PR_AGENT_LIVE_INSTALLATION_ID"), 10, 64)
	olderHead, headErr := domain.ParseHeadSHA(os.Getenv("PR_AGENT_LIVE_APPROVAL_OLDER_HEAD"))
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || numberErr != nil || number <= 0 || installationErr != nil || installation <= 0 || headErr != nil {
		t.Fatal("The live approval regression requires explicit repository, pull request, installation, and older commit identifiers.")
	}
	return domain.PullRequestRef{Repository: domain.Repository{Owner: parts[0], Name: parts[1]}, Number: number, InstallationID: installation, Head: ""}, olderHead
}

func liveApprovalBool(value bool) *bool {
	return &value
}

func liveApprovalRun(t *testing.T, ctx context.Context, service *review.Service, reference domain.PullRequestRef, policy *bool) (domain.ReviewJob, domain.AssessmentOutcome, error) {
	t.Helper()
	var job domain.ReviewJob
	job.PullRequestRef = reference
	job.DeliveryID = "live-approval-publication-" + rand.Text()
	job.RefreshVerdict = true
	job.Settings.ApproveEachCommit = policy
	job, admitted, err := service.Admit(ctx, job)
	if err != nil || !admitted {
		t.Fatal("The production service did not admit the test verdict refresh.")
	}
	outcome, err := service.RunWithOutcome(ctx, job)
	return job, outcome, err
}

func liveApprovalReviews(t *testing.T, ctx context.Context, github *githubapp.Client, reference domain.PullRequestRef) []githubapp.Review {
	t.Helper()
	items, err := github.ListReviews(ctx, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil {
		t.Fatal("The test pull request reviews could not be read.")
	}
	return items
}

func liveApprovalSummary(t *testing.T, ctx context.Context, github *githubapp.Client, reference domain.PullRequestRef, bot string) githubapp.IssueComment {
	t.Helper()
	comments, err := github.ListIssueComments(ctx, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil {
		t.Fatal("The test pull request comments could not be read.")
	}
	var summaries []githubapp.IssueComment
	for _, item := range comments {
		if item.Author == bot && (marker.HasSummary(item.Body) || marker.HasState(item.Body)) {
			summaries = append(summaries, item)
		}
	}
	if len(summaries) != 1 {
		t.Fatal("The test pull request requires exactly one existing bot summary for restoration.")
	}
	return summaries[0]
}

func liveApprovalAssertProtectedReviews(t *testing.T, before, after []githubapp.Review) {
	t.Helper()
	for _, old := range before {
		if !slices.Contains(after, old) {
			t.Errorf("Existing review %d changed during the live approval regression.", old.ID)
		}
	}
}

func liveApprovalAssertSummary(t *testing.T, summary githubapp.IssueComment, summaryID int64, head domain.HeadSHA, revision, invalidCredential string) {
	t.Helper()
	state, valid := marker.DecodeState(summary.Body)
	checkpointHead, marked := marker.FindReview(summary.Body)
	if summary.ID != summaryID || !valid || state.Status != marker.StateDone || state.LastReviewed != head || len(state.Pending) != 0 || len(state.Unread) != 0 || !marked || checkpointHead != head || strings.Contains(summary.Body, invalidCredential) {
		t.Fatal("The single summary did not retain the completed current-commit checkpoint.")
	}
	const prefix = "<!-- pr-review-agent:metadata:v1 "
	start := strings.LastIndex(summary.Body, prefix)
	if start < 0 {
		t.Fatal("The single summary lost its metadata checkpoint.")
	}
	encoded, _, found := strings.Cut(summary.Body[start+len(prefix):], " -->")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	var metadata liveApprovalMetadata
	if !found || err != nil || json.Unmarshal(decoded, &metadata) != nil || metadata.Head != head || metadata.Revision != revision {
		t.Fatal("The single summary did not retain decodable current-commit metadata.")
	}
}

func liveApprovalAssertNoProviderRequests(t *testing.T, logPath string) {
	t.Helper()
	file, err := os.Open(logPath)
	if err != nil {
		t.Fatal("The private service log could not be read.")
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error("The private service log reader could not be closed.")
		}
	}()
	decoder := json.NewDecoder(file)
	for {
		var record liveFailureProviderRecord
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal("The private service log contains an invalid structured record.")
		}
		if record.Cause != "" || record.APIStatus != 0 {
			t.Fatal("The completed checkpoint refresh unexpectedly attempted a provider request.")
		}
	}
}

package reconcile_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reconcile"
)

const (
	testCurrentHead = "c5e6f2dbd8f606cd935815c0e3b2f2202741ed99"
	testFindingHead = "a3c4f1cac7f595bc824704b9d2a1f1191630dc32"
	testBotLogin    = "test-review-agent[bot]"
	testRepoOwner   = "agoodkind"
	testRepoName    = "pr-review-agent"
)

func TestEndToEndReconciliationFailureIsolation(t *testing.T) {
	finding := sampleFinding()
	body, err := marker.EncodeFindingBody(domain.HeadSHA(testFindingHead), finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}

	github := &fakeGitHub{
		head: domain.HeadSHA(testCurrentHead),
		threads: []githubapp.ReviewThread{
			ownedThread("thread-owned", body, finding, false),
		},
		files: map[string][]byte{
			finding.Path: []byte("line1\nissue line\nline3\n"),
		},
		compareFiles: []githubapp.ChangedFile{{
			Path:         finding.Path,
			Patch:        "@@ -1,3 +1,3 @@\n line1\n-issue line\n+fixed line\n line3\n",
			PatchPresent: true,
		}},
	}
	model := &fakeModel{reconcileErr: errors.New("reconcile failed")}

	service := reconcile.NewService(github, model, testBotLogin, nil)
	if _, err := service.Reconcile(context.Background(), testJob()); err == nil {
		t.Fatal("Reconcile: want error")
	}
	if len(github.resolveCalls) != 0 {
		t.Fatalf("resolve calls = %v, want none after model failure", github.resolveCalls)
	}
}

func TestReconcileSelectsOnlyUnresolvedOwnedMarkedThreads(t *testing.T) {
	finding := sampleFinding()
	ownedBody, err := marker.EncodeFindingBody(domain.HeadSHA(testFindingHead), finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}

	github := &fakeGitHub{
		head: domain.HeadSHA(testCurrentHead),
		threads: []githubapp.ReviewThread{
			ownedThread("thread-owned", ownedBody, finding, false),
			{NodeID: "thread-resolved", Resolved: true, RootComment: botComment(ownedBody, finding)},
			{NodeID: "thread-foreign", Resolved: false, RootComment: foreignComment(ownedBody, finding)},
		},
		files: map[string][]byte{
			finding.Path: []byte("line1\nissue line\nline3\n"),
		},
		compareFiles: []githubapp.ChangedFile{{
			Path:         finding.Path,
			Status:       "modified",
			Patch:        "@@ -1,3 +1,3 @@\n line1\n-issue line\n+fixed line\n line3\n",
			PatchPresent: true,
		}},
	}
	model := &fakeModel{
		resolutions: []domain.ThreadResolution{{
			ThreadNodeID: "thread-owned",
			Resolution:   domain.ResolutionResolved,
			Reason:       "fixed",
		}},
	}

	service := reconcile.NewService(github, model, testBotLogin, nil)
	job := testJob()
	if _, err := service.Reconcile(context.Background(), job); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(model.prompts) != 1 {
		t.Fatalf("model prompt count = %d, want 1", len(model.prompts))
	}
	if !strings.Contains(model.prompts[0], "thread-owned") {
		t.Fatalf("prompt missing owned thread: %q", model.prompts[0])
	}
	if len(github.resolveCalls) != 1 || github.resolveCalls[0] != "thread-owned" {
		t.Fatalf("resolve calls = %v, want [thread-owned]", github.resolveCalls)
	}
}

func TestReconcileIgnoresForeignMalformedAndCurrentHeadThreads(t *testing.T) {
	finding := sampleFinding()
	oldBody, err := marker.EncodeFindingBody(domain.HeadSHA(testFindingHead), finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody old: %v", err)
	}
	currentBody, err := marker.EncodeFindingBody(domain.HeadSHA(testCurrentHead), finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody current: %v", err)
	}

	github := &fakeGitHub{
		head: domain.HeadSHA(testCurrentHead),
		threads: []githubapp.ReviewThread{
			{NodeID: "thread-foreign", Resolved: false, RootComment: foreignComment(oldBody, finding)},
			{NodeID: "thread-malformed", Resolved: false, RootComment: botComment("not a finding body", finding)},
			{NodeID: "thread-current", Resolved: false, RootComment: botComment(currentBody, finding)},
			{NodeID: "thread-deleted", Resolved: false, RootComment: domain.ReviewComment{Author: testBotLogin, Body: ""}},
		},
	}
	model := &fakeModel{}

	service := reconcile.NewService(github, model, testBotLogin, nil)
	if _, err := service.Reconcile(context.Background(), testJob()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(model.prompts) != 0 {
		t.Fatalf("model prompt count = %d, want 0", len(model.prompts))
	}
	if len(github.resolveCalls) != 0 {
		t.Fatalf("resolve calls = %v, want none", github.resolveCalls)
	}
}

func TestReconcileResolvesOnlyProvenFixedOrInvalidFindings(t *testing.T) {
	finding := sampleFinding()
	body, err := marker.EncodeFindingBody(domain.HeadSHA(testFindingHead), finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}

	github := &fakeGitHub{
		head: domain.HeadSHA(testCurrentHead),
		threads: []githubapp.ReviewThread{
			ownedThread("thread-resolved", body, finding, false),
			ownedThread("thread-open", body, finding, false),
		},
		files: map[string][]byte{
			finding.Path: []byte("line1\nissue line\nline3\n"),
		},
		compareFiles: []githubapp.ChangedFile{{
			Path:         finding.Path,
			Patch:        "@@ -1,3 +1,3 @@\n line1\n-issue line\n+fixed line\n line3\n",
			PatchPresent: true,
		}},
	}
	model := &fakeModel{
		resolutions: []domain.ThreadResolution{
			{ThreadNodeID: "thread-resolved", Resolution: domain.ResolutionResolved, Reason: "fixed"},
			{ThreadNodeID: "thread-open", Resolution: domain.ResolutionOpen, Reason: "still applies"},
		},
	}

	service := reconcile.NewService(github, model, testBotLogin, nil)
	threads, err := service.Reconcile(context.Background(), testJob())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(github.resolveCalls) != 1 || github.resolveCalls[0] != "thread-resolved" {
		t.Fatalf("resolve calls = %v, want [thread-resolved]", github.resolveCalls)
	}
	if !threads[0].Resolved || threads[1].Resolved {
		t.Fatalf("resolved states = [%t %t], want [true false]", threads[0].Resolved, threads[1].Resolved)
	}
}

func TestReconcileLeavesOpenAndUncertainFindingsOpen(t *testing.T) {
	finding := sampleFinding()
	body, err := marker.EncodeFindingBody(domain.HeadSHA(testFindingHead), finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}

	github := &fakeGitHub{
		head: domain.HeadSHA(testCurrentHead),
		threads: []githubapp.ReviewThread{
			ownedThread("thread-open", body, finding, false),
			ownedThread("thread-uncertain", body, finding, false),
		},
		files: map[string][]byte{
			finding.Path: []byte("line1\nissue line\nline3\n"),
		},
		compareFiles: []githubapp.ChangedFile{{
			Path:         finding.Path,
			Patch:        "@@ -1,1 +1,1 @@\n context\n",
			PatchPresent: true,
		}},
	}
	model := &fakeModel{
		resolutions: []domain.ThreadResolution{
			{ThreadNodeID: "thread-open", Resolution: domain.ResolutionOpen, Reason: "still applies"},
			{ThreadNodeID: "thread-uncertain", Resolution: domain.ResolutionUncertain, Reason: "unclear"},
		},
	}

	service := reconcile.NewService(github, model, testBotLogin, nil)
	if _, err := service.Reconcile(context.Background(), testJob()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(github.resolveCalls) != 0 {
		t.Fatalf("resolve calls = %v, want none", github.resolveCalls)
	}
}

func TestReconcilePostsNoReplies(t *testing.T) {
	finding := sampleFinding()
	body, err := marker.EncodeFindingBody(domain.HeadSHA(testFindingHead), finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}

	github := &fakeGitHub{
		head: domain.HeadSHA(testCurrentHead),
		threads: []githubapp.ReviewThread{
			ownedThread("thread-one", body, finding, false),
		},
		files: map[string][]byte{
			finding.Path: []byte("line1\nissue line\nline3\n"),
		},
		compareFiles: []githubapp.ChangedFile{{
			Path:         finding.Path,
			Patch:        "@@ -1,3 +1,3 @@\n line1\n-issue line\n+fixed line\n line3\n",
			PatchPresent: true,
		}},
	}
	model := &fakeModel{
		resolutions: []domain.ThreadResolution{{
			ThreadNodeID: "thread-one",
			Resolution:   domain.ResolutionResolved,
			Reason:       "fixed",
		}},
	}

	service := reconcile.NewService(github, model, testBotLogin, nil)
	if _, err := service.Reconcile(context.Background(), testJob()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if github.replyCalls != 0 {
		t.Fatalf("reply calls = %d, want 0", github.replyCalls)
	}
}

func TestReconcileContinuesAfterOneThreadFailure(t *testing.T) {
	finding := sampleFinding()
	body, err := marker.EncodeFindingBody(domain.HeadSHA(testFindingHead), finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}

	github := &fakeGitHub{
		head: domain.HeadSHA(testCurrentHead),
		threads: []githubapp.ReviewThread{
			ownedThread("thread-fail", body, finding, false),
			ownedThread("thread-ok", body, finding, false),
		},
		files: map[string][]byte{
			finding.Path: []byte("line1\nissue line\nline3\n"),
		},
		compareFiles: []githubapp.ChangedFile{{
			Path:         finding.Path,
			Patch:        "@@ -1,3 +1,3 @@\n line1\n-issue line\n+fixed line\n line3\n",
			PatchPresent: true,
		}},
		resolveErrors: map[string]error{
			"thread-fail": errors.New("resolve failed"),
		},
	}
	model := &fakeModel{
		resolutions: []domain.ThreadResolution{
			{ThreadNodeID: "thread-fail", Resolution: domain.ResolutionResolved, Reason: "fixed"},
			{ThreadNodeID: "thread-ok", Resolution: domain.ResolutionResolved, Reason: "fixed"},
		},
	}

	service := reconcile.NewService(github, model, testBotLogin, nil)
	_, err = service.Reconcile(context.Background(), testJob())
	if err == nil {
		t.Fatal("Reconcile: want aggregate error")
	}
	if !strings.Contains(err.Error(), "thread-fail") {
		t.Fatalf("error = %q, want thread-fail failure", err.Error())
	}
	if len(github.resolveCalls) != 2 {
		t.Fatalf("resolve calls = %v, want two attempts", github.resolveCalls)
	}
	if github.resolveCalls[1] != "thread-ok" {
		t.Fatalf("second resolve call = %q, want thread-ok", github.resolveCalls[1])
	}
}

func TestReconcileStopsMutationsWhenHeadChanges(t *testing.T) {
	finding := sampleFinding()
	body, err := marker.EncodeFindingBody(domain.HeadSHA(testFindingHead), finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}

	staleHead := domain.HeadSHA(testCurrentHead)
	newHead := domain.HeadSHA("d6f7a3ece91717de046926d1f4c3a3313852fe00")

	github := &fakeGitHub{
		head:           staleHead,
		headAfterModel: newHead,
		threads: []githubapp.ReviewThread{
			ownedThread("thread-one", body, finding, false),
			ownedThread("thread-two", body, finding, false),
		},
		files: map[string][]byte{
			finding.Path: []byte("line1\nissue line\nline3\n"),
		},
		compareFiles: []githubapp.ChangedFile{{
			Path:         finding.Path,
			Patch:        "@@ -1,3 +1,3 @@\n line1\n-issue line\n+fixed line\n line3\n",
			PatchPresent: true,
		}},
	}
	model := &fakeModel{
		resolutions: []domain.ThreadResolution{
			{ThreadNodeID: "thread-one", Resolution: domain.ResolutionResolved, Reason: "fixed"},
			{ThreadNodeID: "thread-two", Resolution: domain.ResolutionResolved, Reason: "fixed"},
		},
	}

	service := reconcile.NewService(github, model, testBotLogin, nil)
	_, err = service.Reconcile(context.Background(), testJob())
	if err == nil {
		t.Fatal("Reconcile: want head changed error")
	}
	if !strings.Contains(err.Error(), "head changed") {
		t.Fatalf("error = %q, want head changed", err.Error())
	}
	if len(github.resolveCalls) != 0 {
		t.Fatalf("resolve calls = %v, want none after head change", github.resolveCalls)
	}
}

func TestReconcileResolvesRemovedFindingAnchorWithoutModel(t *testing.T) {
	finding := sampleFinding()
	body, err := marker.EncodeFindingBody(domain.HeadSHA(testFindingHead), finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}

	github := &fakeGitHub{
		head: domain.HeadSHA(testCurrentHead),
		threads: []githubapp.ReviewThread{
			ownedThread("thread-missing", body, finding, false),
		},
		files: map[string][]byte{
			finding.Path: []byte("line1"),
		},
	}
	model := &fakeModel{}

	service := reconcile.NewService(github, model, testBotLogin, nil)
	threads, err := service.Reconcile(context.Background(), testJob())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(model.prompts) != 0 {
		t.Fatalf("model prompt count = %d, want 0 for removed anchor", len(model.prompts))
	}
	if len(github.resolveCalls) != 1 || github.resolveCalls[0] != "thread-missing" {
		t.Fatalf("resolve calls = %v, want [thread-missing]", github.resolveCalls)
	}
	if !threads[0].Resolved {
		t.Fatal("thread remains unresolved")
	}
}

func TestReconcileFollowsRenamedFindingFile(t *testing.T) {
	finding := sampleFinding()
	body, err := marker.EncodeFindingBody(domain.HeadSHA(testFindingHead), finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}

	const renamedPath = "internal/app/server.go"
	github := &fakeGitHub{
		head: domain.HeadSHA(testCurrentHead),
		threads: []githubapp.ReviewThread{
			ownedThread("thread-renamed", body, finding, false),
		},
		files: map[string][]byte{
			renamedPath: []byte("line1\nissue line\nline3"),
		},
		compareFiles: []githubapp.ChangedFile{{
			Path:         renamedPath,
			PreviousPath: finding.Path,
			Status:       "renamed",
			Patch:        "@@ -1,3 +1,3 @@\n line1\n issue line\n line3\n",
			PatchPresent: true,
		}},
	}
	model := &fakeModel{
		resolutions: []domain.ThreadResolution{{
			ThreadNodeID: "thread-renamed",
			Resolution:   domain.ResolutionOpen,
			Reason:       "defect remains after rename",
		}},
	}

	service := reconcile.NewService(github, model, testBotLogin, nil)
	if _, err := service.Reconcile(context.Background(), testJob()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(model.prompts) != 1 || !strings.Contains(model.prompts[0], "issue line") {
		t.Fatalf("model prompts = %q, want renamed file context", model.prompts)
	}
	if len(github.resolveCalls) != 0 {
		t.Fatalf("resolve calls = %v, want none", github.resolveCalls)
	}
}

func sampleFinding() domain.Finding {
	return domain.Finding{
		Path:       "internal/app/handler.go",
		StartLine:  2,
		EndLine:    2,
		Title:      "Missing validation",
		Body:       "Validate the webhook payload before enqueue.",
		Importance: 7,
	}
}

func testJob() domain.ReviewJob {
	return domain.ReviewJob{
		DeliveryID: "delivery-1",
		PullRequestRef: domain.PullRequestRef{
			Repository: domain.Repository{
				Owner: testRepoOwner,
				Name:  testRepoName,
			},
			Number:         42,
			InstallationID: 99,
			Head:           domain.HeadSHA(testCurrentHead),
		},
	}
}

func ownedThread(
	nodeID string,
	body string,
	finding domain.Finding,
	resolved bool,
) githubapp.ReviewThread {
	return githubapp.ReviewThread{
		NodeID:      nodeID,
		Resolved:    resolved,
		RootComment: botComment(body, finding),
	}
}

func botComment(body string, finding domain.Finding) domain.ReviewComment {
	return domain.ReviewComment{
		DatabaseID: 1,
		Author:     testBotLogin,
		Body:       body,
		Path:       finding.Path,
		StartLine:  finding.StartLine,
		EndLine:    finding.EndLine,
	}
}

func foreignComment(body string, finding domain.Finding) domain.ReviewComment {
	comment := botComment(body, finding)
	comment.Author = "other-user"
	return comment
}

type fakeGitHub struct {
	head           domain.HeadSHA
	headAfterModel domain.HeadSHA
	threads        []githubapp.ReviewThread
	files          map[string][]byte
	getFileErrors  map[string]error
	compareFiles   []githubapp.ChangedFile
	compareError   error
	resolveErrors  map[string]error
	resolveCalls   []string
	replyCalls     int
	getPullCount   int
}

func (fake *fakeGitHub) ListReviewThreads(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	_ int,
) ([]githubapp.ReviewThread, error) {
	return fake.threads, nil
}

func (fake *fakeGitHub) GetFile(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	path string,
	_ domain.HeadSHA,
) ([]byte, error) {
	if err := fake.getFileErrors[path]; err != nil {
		return nil, err
	}
	content, ok := fake.files[path]
	if !ok {
		return nil, fmt.Errorf("file %s not found", path)
	}
	return content, nil
}

func (fake *fakeGitHub) Compare(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	_, _ domain.HeadSHA,
) ([]githubapp.ChangedFile, error) {
	if fake.compareError != nil {
		return nil, fake.compareError
	}
	return fake.compareFiles, nil
}

func (fake *fakeGitHub) GetPullRequest(
	_ context.Context,
	_ int64,
	_ domain.Repository,
	_ int,
) (githubapp.PullRequest, error) {
	fake.getPullCount++
	head := fake.head
	if fake.headAfterModel != "" && fake.getPullCount > 1 {
		head = fake.headAfterModel
	}
	return githubapp.PullRequest{Number: 42, Head: head}, nil
}

func (fake *fakeGitHub) ResolveReviewThread(
	_ context.Context,
	_ int64,
	threadNodeID string,
) error {
	fake.resolveCalls = append(fake.resolveCalls, threadNodeID)
	if err := fake.resolveErrors[threadNodeID]; err != nil {
		return err
	}
	return nil
}

type fakeModel struct {
	prompts      []string
	resolutions  []domain.ThreadResolution
	reconcileErr error
}

func (fake *fakeModel) Reconcile(_ context.Context, prompt string) ([]domain.ThreadResolution, error) {
	fake.prompts = append(fake.prompts, prompt)
	if fake.reconcileErr != nil {
		return nil, fake.reconcileErr
	}
	return fake.resolutions, nil
}

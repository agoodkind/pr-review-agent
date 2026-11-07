package review_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/review"
)

const inspectionActivationSource = "#!/usr/bin/env bash\nset -euo pipefail\nsysctl --load=/etc/sysctl.d/forwarding.conf\nnetworkctl reload\n"

type inspectionServiceCapture struct {
	mu                 sync.Mutex
	reviewRequests     []string
	requests           int
	revisions          []string
	archivesAtReview   []int
	forbiddenRequests  []string
	beforeContinuation func()
	snapshotDirectory  string
}

func TestServiceInspectionLoadsUnchangedSourceWithoutAddingFindingAnchors(t *testing.T) {
	fixture, job, capture := newInspectionServiceFixture(t, "complete")
	if err := fixture.run(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	assertInspectionSnapshotRemoved(t, capture)
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.reviewRequests) != 2 {
		t.Fatalf("The real SDK sent %d review requests; expected an inspection request and its continuation.", len(capture.reviewRequests))
	}
	if strings.Contains(capture.reviewRequests[0], "sysctl --load") || !strings.Contains(capture.reviewRequests[1], "sysctl --load") || !strings.Contains(capture.reviewRequests[1], string(job.Head)) || strings.Contains(capture.reviewRequests[1], "uncommitted activation source") {
		t.Fatal("The continuation must receive the unchanged activation script from the reviewed commit.")
	}
	if len(capture.revisions) != 1 || capture.revisions[0] != string(job.Head) {
		t.Fatalf("Collection and inspection did not reuse one immutable repository archive: %v", capture.revisions)
	}
	if len(capture.archivesAtReview) != 2 || capture.archivesAtReview[0] != 1 || capture.archivesAtReview[1] != 1 || len(capture.forbiddenRequests) != 0 {
		t.Fatalf("Review source or search used individual APIs instead of the existing snapshot: archives=%v forbidden=%v", capture.archivesAtReview, capture.forbiddenRequests)
	}
	if len(fixture.state.streamedComments) != 1 || fixture.state.streamedComments[0]["path"] != "network.tf" || fixture.state.streamedComments[0]["line"] != float64(3) {
		t.Fatalf("Supporting source created a finding anchor or lost the changed-line finding: %v", fixture.state.streamedComments)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) || fixture.state.lastUpdateCheckRun["conclusion"] != "success" {
		t.Fatalf("The complete inspected review did not publish its grounded rejection: review=%v check=%v", fixture.state.lastSubmitReview, fixture.state.lastUpdateCheckRun)
	}
	body := failureSummaryComment(t, fixture)
	for _, row := range []string{
		"| Requests | `" + strconv.Itoa(capture.requests) + "` |",
		"| Total tokens | `" + strconv.Itoa(capture.requests*16) + "` |",
		"| All file content inspected | yes |",
	} {
		if !strings.Contains(body, row) {
			t.Fatalf("The completed review omitted its full request usage or coverage: %s", row)
		}
	}
	state := decodedSummaryState(t, fixture)
	if state.LastReviewed != job.Head || len(state.Pending) != 0 {
		t.Fatal("The inspected review did not complete its original source checkpoint.")
	}
}

func TestServiceInspectionFailurePreservesFindingsAndPendingWork(t *testing.T) {
	fixture, job, capture := newInspectionServiceFixture(t, "missing")
	_ = fixture.run(t.Context(), job)
	assertInspectionSnapshotRemoved(t, capture)
	if len(fixture.state.streamedComments) != 1 || fixture.state.streamedComments[0]["path"] != "network.tf" {
		t.Fatal("The required evidence failure discarded a finding established before the inspection.")
	}
	for _, submitted := range fixture.state.submittedReviews {
		if submitted["state"] == "APPROVED" {
			t.Fatal("A required inspection failure published an approval.")
		}
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] == "success" {
		t.Fatal("A required inspection failure satisfied the review check.")
	}
	state := decodedSummaryState(t, fixture)
	if state.LastReviewed == job.Head || len(state.Pending) == 0 {
		t.Fatal("A required inspection failure completed the unread source checkpoint.")
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.reviewRequests) != 1 {
		t.Fatal("The service continued model analysis without the required inspection evidence.")
	}
}

func TestServiceInspectionCancelsPublicationAfterTheHeadChanges(t *testing.T) {
	fixture, job, capture := newInspectionServiceFixture(t, "head_moved")
	if err := fixture.run(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	assertInspectionSnapshotRemoved(t, capture)
	if len(fixture.state.streamedComments) != 0 || len(fixture.state.submittedReviews) != 0 || fixture.state.lastUpdateCheckRun["conclusion"] != "cancelled" {
		t.Fatalf("The head guard allowed stale inspected findings to publish: comments=%v reviews=%v check=%v", fixture.state.streamedComments, fixture.state.submittedReviews, fixture.state.lastUpdateCheckRun)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.revisions) != 1 || capture.revisions[0] != string(job.Head) {
		t.Fatal("A head change altered the pinned inspection revision.")
	}
}

func newInspectionServiceFixture(t *testing.T, scenario string) (*serviceFixture, domain.ReviewJob, *inspectionServiceCapture) {
	t.Helper()
	snapshotDirectory := t.TempDir()
	t.Setenv("TMPDIR", snapshotDirectory)
	directory := t.TempDir()
	relationshipGitCommand(t, directory, "init", "--quiet")
	if err := os.Mkdir(filepath.Join(directory, "network"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := "resource \"terraform_data\" \"network\" {\n  input = {\n    bridge = \"vmbr1\"\n  }\n  provisioner \"remote-exec\" {\n    script = \"${path.module}/network/activate.sh\"\n  }\n}\n"
	for path, content := range map[string]string{"network.tf": before, "network/activate.sh": inspectionActivationSource} {
		if scenario == "missing" && path == "network/activate.sh" {
			continue
		}
		if err := os.WriteFile(filepath.Join(directory, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(subject string) {
		t.Helper()
		relationshipGitCommand(t, directory, "add", "--all")
		relationshipGitCommand(t, directory, "-c", "user.name=Integration Test", "-c", "user.email=integration@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "-m", subject+"\n\nCo-authored-by: Codex <noreply@openai.com>")
	}
	commit("Create host network source")
	base := strings.TrimSpace(relationshipGitCommand(t, directory, "rev-parse", "HEAD"))
	after := strings.Replace(before, "vmbr1", "uplink", 1)
	if err := os.WriteFile(filepath.Join(directory, "network.tf"), []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	commit("Rename the upstream bridge")
	head := strings.TrimSpace(relationshipGitCommand(t, directory, "rev-parse", "HEAD"))
	patch := relationshipGitCommand(t, directory, "diff", "--unified=3", base, head, "--", "network.tf")
	if err := os.WriteFile(filepath.Join(directory, "network/activate.sh"), []byte("uncommitted activation source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newServiceFixture(t, serviceFixtureOptions{})
	fixture.state.headSHA = head
	capture := &inspectionServiceCapture{snapshotDirectory: snapshotDirectory}
	if scenario == "head_moved" {
		capture.beforeContinuation = func() {
			fixture.state.mu.Lock()
			fixture.state.headSHA = testStaleHeadSHA
			fixture.state.mu.Unlock()
		}
	}
	githubServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			switch {
			case isPullRequestRead(request):
				fixture.state.mu.Lock()
				currentHead := fixture.state.headSHA
				fixture.state.mu.Unlock()
				serviceWriteJSON(writer, http.StatusOK, map[string]any{"number": testPRNumber, "state": "open", "merged": false, "draft": false, "title": "Rename the upstream bridge", "body": "The host requires the renamed bridge during network activation.", "commits": 0, "head": map[string]string{"sha": currentHead}, "base": map[string]string{"sha": base}})
				return
			case strings.HasSuffix(request.URL.Path, "/files"):
				serviceWriteJSON(writer, http.StatusOK, []map[string]string{{"filename": "network.tf", "status": "modified", "patch": patch}})
				return
			case strings.HasSuffix(request.URL.Path, "/commits"):
				serviceWriteJSON(writer, http.StatusOK, []any{})
				return
			case strings.Contains(request.URL.Path, "/contents/") || request.URL.Path == "/search/code":
				capture.mu.Lock()
				capture.forbiddenRequests = append(capture.forbiddenRequests, request.URL.RequestURI())
				capture.mu.Unlock()
				t.Errorf("The review requested an individual source API: %s", request.URL.RequestURI())
				http.Error(writer, "Use the pinned repository archive.", http.StatusInternalServerError)
				return
			case strings.Contains(request.URL.Path, "/tarball/"):
				_, revision, _ := strings.Cut(request.URL.Path, "/tarball/")
				capture.mu.Lock()
				capture.revisions = append(capture.revisions, revision)
				capture.mu.Unlock()
				command := exec.CommandContext(request.Context(), "git", "archive", "--format=tar.gz", "--prefix=fixture-"+revision+"/", revision)
				command.Dir = directory
				content, err := command.Output()
				if err != nil {
					http.Error(writer, "source does not exist at this revision", http.StatusNotFound)
					return
				}
				writer.Header().Set("Content-Type", "application/gzip")
				_, _ = writer.Write(content)
				return
			}
		}
		fixture.state.mu.Lock()
		defer fixture.state.mu.Unlock()
		handleServiceRequest(writer, request, fixture.state)
	}))
	t.Cleanup(githubServer.Close)
	providerServer := inspectionServiceProvider(t, capture, scenario)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	apiAddress, err := url.Parse(githubServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	graphqlAddress, err := url.Parse(githubServer.URL + "/graphql")
	if err != nil {
		t.Fatal(err)
	}
	providerAddress, err := url.Parse(providerServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	policy := policytest.Load(t)
	examplePrivateKey := serviceTestPrivateKey(t)
	github := githubapp.NewClient(config.Config{GitHubAppID: testGitHubAppID, GitHubPrivateKey: examplePrivateKey, GitHubAPIBaseURL: apiAddress, GitHubGraphQLURL: graphqlAddress}, githubServer.Client(), func() time.Time { return time.Unix(1_700_000_000, 0) }, logger)
	model := openai.NewClient(config.Config{ReviewPolicy: policy, MinimumImportance: 5, Providers: []config.ProviderConfig{{ID: "inspection", API: config.ResponsesAPI, Model: "inspection-service-model", APIKey: "example-credential", BaseURL: providerAddress}}}, providerServer.Client())
	fixture.github = github
	fixture.model = model
	fixture.service = review.NewService(github, diff.NewCollector(github), model, fixture.reconciler, queue.NewKeyedLocker(), testBotLogin, 5, config.DefaultReviewMaxFiles, config.DefaultReviewMaxChunks, config.DefaultReviewChunkTimeout, nil, testClock(8*time.Second), logger, 1, config.MaximumPromptBytes, policy)
	job := fixture.job()
	job.Head = domain.HeadSHA(head)
	return fixture, job, capture
}

func assertInspectionSnapshotRemoved(t *testing.T, capture *inspectionServiceCapture) {
	t.Helper()
	entries, err := os.ReadDir(capture.snapshotDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("The completed service run retained repository snapshot files: %v", entries)
	}
}

func inspectionServiceProvider(t *testing.T, capture *inspectionServiceCapture, scenario string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		var native struct {
			Text struct {
				Format struct {
					Name string `json:"name"`
				} `json:"format"`
			} `json:"text"`
		}
		if err := json.Unmarshal(payload, &native); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		capture.mu.Lock()
		capture.requests++
		requestIndex := capture.requests
		var content []byte
		switch native.Text.Format.Name {
		case "review_result":
			capture.reviewRequests = append(capture.reviewRequests, string(payload))
			capture.archivesAtReview = append(capture.archivesAtReview, len(capture.revisions))
			iteration := len(capture.reviewRequests)
			finding := domain.Finding{RuleID: "functional_correctness", Surface: domain.FindingFile, Path: "network.tf", StartLine: 3, EndLine: 3, Title: "Create the renamed bridge before activation", Body: "The activation script loads settings before creating the renamed bridge. Create the bridge before loading its settings.", Claim: "The activation script requires the renamed bridge before it exists.", Evidence: "bridge = \"uplink\"", Importance: 9}
			var result domain.ReviewResult
			result.OmissionsAcceptable = true
			var inspections []review.InspectionRequest
			if iteration == 1 {
				inspections = []review.InspectionRequest{{Operation: "search_repository", Query: "sysctl --load", Reason: "Find activation settings in the reviewed repository."}, {Operation: "read_file", Target: "network/activate.sh", Reason: "Check whether activation creates the renamed bridge before loading its settings."}}
				if scenario == "missing" {
					result.Findings = []domain.Finding{finding}
				}
			} else {
				if capture.beforeContinuation != nil {
					capture.beforeContinuation()
				}
				result.Findings = []domain.Finding{finding}
				unsupported := finding
				unsupported.Path = "network/activate.sh"
				unsupported.StartLine = 3
				unsupported.EndLine = 3
				unsupported.Evidence = "sysctl --load=/etc/sysctl.d/forwarding.conf"
				result.Findings = append(result.Findings, unsupported)
			}
			content, err = json.Marshal(struct {
				domain.ReviewResult
				InspectionRequests []review.InspectionRequest `json:"inspection_requests"`
			}{ReviewResult: result, InspectionRequests: inspections})
		case "review_report":
			content, err = json.Marshal(review.Report{Summary: "The change renames the upstream bridge.", Walkthrough: []string{"The host activation uses the renamed bridge."}})
		case "finding_consolidation":
			content = []byte(`{"groups":[{"candidates":[1],"restates_open_thread":false,"reason":"The candidate identifies a distinct source defect."}]}`)
		default:
			err = fmt.Errorf("unexpected model stage %s", native.Text.Format.Name)
		}
		capture.mu.Unlock()
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		writeInspectionServiceCompletion(writer, string(content), requestIndex)
	}))
	t.Cleanup(server.Close)
	return server
}

func writeInspectionServiceCompletion(writer http.ResponseWriter, content string, requestIndex int) {
	writer.Header().Set("Content-Type", "text/event-stream")
	frames := []map[string]any{
		{"type": "response.output_text.delta", "delta": content, "output_index": 0, "content_index": 0},
		{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("inspection-response-%d", requestIndex), "object": "response", "status": "completed", "model": "inspection-service-model", "output": []map[string]any{{"type": "message", "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": content, "annotations": []any{}}}}}, "usage": map[string]int{"input_tokens": 11, "output_tokens": 5, "total_tokens": 16}}},
	}
	for _, frame := range frames {
		encoded, err := json.Marshal(frame)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(writer, "data: %s\n\n", encoded)
	}
}

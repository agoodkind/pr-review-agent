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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestLiveRoutingOutputInterruptionPreservesSavedExplanation(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_ROUTING_OUTPUT_TEST") != "1" {
		t.Skip("The live routing output regression is not enabled.")
	}
	opts := liveReviewBodyOptions(t)
	if opts.repository != "agoodkind/goodkind.io" || opts.pullRequest != 354 || opts.installationID != 153224986 {
		t.Fatal("The routing output regression requires the dedicated goodkind.io fixture 354.")
	}
	cfg, _ := liveFailureConfig(t)
	actual := liveFailoverConfig(t)
	provider := actual.Providers[0]
	if provider.ID != "openrouter" || provider.Model != "openrouter/free" {
		t.Fatal("The routing output regression requires the configured OpenRouter free router.")
	}
	if endpoint := os.Getenv("PR_AGENT_LIVE_BASE_URL"); endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			t.Fatal("The optional live relay requires a valid HTTPS base URL without credentials, a query, or a fragment.")
		}
		provider.BaseURL = parsed
	}
	provider.MaxOutputTokens = 16
	provider.OmitMaxOutputTokens = false
	provider.OmitTextFormat = true
	cfg.Providers = actual.Providers
	cfg.Providers[0] = provider
	cfg.ProviderBudgetURL = nil
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	preflight := gklog.WithLogger(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	_, preflightErr := openai.NewClient(cfg, http.DefaultClient).Review(preflight, liveFailoverPrompt(t, cfg))
	var truncated *openai.TruncatedError
	if !errors.As(preflightErr, &truncated) {
		t.Fatal("The actual provider did not establish token interruption before GitHub mutation.")
	}
	directory, err := os.MkdirTemp("", "pragent-routing-output-live-")
	if err != nil {
		t.Fatal("The private evidence directory could not be created.")
	}
	logPath := filepath.Join(directory, "service.ndjson")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal("The private service log could not be created.")
	}
	t.Cleanup(func() {
		if err := logFile.Close(); err != nil {
			t.Error("The private service log could not be closed.")
		}
	})
	logger := slog.New(slog.NewJSONHandler(logFile, nil))
	ctx = gklog.WithLogger(ctx, logger)
	service, client := liveFailureService(cfg, logger)
	repository, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal("The explicit fixture repository is invalid.")
	}
	before := liveReviewBodyCapture(t, ctx, client, opts, repository)
	if before.PullRequest.State != "closed" || !before.PullRequest.Draft || !liveReviewBodyFixture(t, opts, before.PullRequest.Title) || githubapp.LatestDecisionReview(before.Reviews, cfg.GitHubBotLogin).State != "APPROVED" {
		t.Fatal("The dedicated fixture requires a closed draft pull request and an existing bot approval.")
	}
	var original githubapp.IssueComment
	for _, comment := range before.Comments {
		if comment.Author == cfg.GitHubBotLogin && marker.HasState(comment.Body) {
			if original.ID != 0 {
				t.Fatal("The fixture contains multiple bot summaries.")
			}
			original = comment
		}
	}
	state, valid := marker.DecodeState(original.Body)
	if original.ID == 0 || !valid || !strings.Contains(original.Body, "<!-- pr-review-agent:metadata:v1 ") {
		t.Fatal("The existing summary requires valid saved state and review metadata.")
	}
	if err := writeReviewBodyBackup(filepath.Join(directory, "before.json"), before); err != nil {
		t.Fatal("The original fixture snapshot could not be saved privately.")
	}
	t.Cleanup(func() {
		recovery, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		if _, err := client.UpdateIssueComment(recovery, opts.installationID, repository, original.ID, original.Body); err != nil {
			t.Error("The original fixture summary could not be restored.")
		}
		after := liveReviewBodyCapture(t, recovery, client, opts, repository)
		if !liveReviewBodySnapshotsEqual(before, after) {
			t.Error("The regression did not restore the original fixture records.")
		}
	})
	explanation := "The fixture title does not identify which review behavior the change verifies. A reviewer cannot distinguish a test of the summary link from a test of historical review repair without reading the description, even though those operations exercise different GitHub records. State the specific review behavior in the title and preserve the description that explains how the fixture checks that behavior."
	if len(explanation) <= 256 {
		t.Fatal("The saved explanation must exceed the previous short finding boundary.")
	}
	originalState := marker.EncodeState(state)
	state.LastReviewed = ""
	state.Status = marker.StateReviewing
	state.RunID = "live-routing-output-setup-" + rand.Text()
	state.Pending = []string{"pending-routing-output-fragment"}
	state.Completed = nil
	record, findings, originalMetadata := liveRoutingMetadata(t, original.Body)
	var previousExplanation string
	if json.Unmarshal(findings[0]["body"], &previousExplanation) != nil || previousExplanation == "" || !strings.Contains(original.Body, previousExplanation) {
		t.Fatal("The saved metadata finding requires its corresponding visible explanation.")
	}
	var ruleID reviewrules.ID
	if json.Unmarshal(findings[0]["rule_id"], &ruleID) != nil {
		t.Fatal("The saved metadata finding requires a rule identifier.")
	}
	if _, err := cfg.ReviewPolicy.Catalog().FindingImportance(ruleID, 10, cfg.RuleImportance); err != nil {
		t.Fatal("The current catalog must support the saved finding rule.")
	}
	findings[0]["body"] = liveFailureEncode(t, explanation)
	record["findings"] = liveFailureEncode(t, findings)
	record["head"] = liveFailureEncode(t, string(before.PullRequest.Head))
	record["revision"] = liveFailureEncode(t, diff.PullRequestRevision(before.PullRequest))
	metadata := "<!-- pr-review-agent:metadata:v1 " + base64.RawURLEncoding.EncodeToString(liveFailureEncode(t, record)) + " -->"
	seeded := strings.Replace(original.Body, originalMetadata, metadata, 1)
	seeded = strings.Replace(seeded, previousExplanation, explanation, 1)
	seeded = strings.Replace(seeded, originalState, marker.EncodeState(state), 1)
	if seeded == original.Body {
		t.Fatal("The saved state marker could not identify the explanation insertion point.")
	}
	if err := os.WriteFile(filepath.Join(directory, "seeded.md"), []byte(seeded), 0o600); err != nil {
		t.Fatal("The seeded summary could not be saved privately.")
	}
	if _, err := client.UpdateIssueComment(ctx, opts.installationID, repository, original.ID, seeded); err != nil {
		t.Fatal("The existing summary could not publish the long explanation.")
	}
	if livePartialSummary(t, ctx, client, opts, original.ID) != seeded {
		t.Fatal("GitHub did not preserve the complete published explanation.")
	}
	job := domain.ReviewJob{
		PullRequestRef: domain.PullRequestRef{Repository: repository, Number: opts.pullRequest, InstallationID: opts.installationID, Head: before.PullRequest.Head},
		DeliveryID:     "live-routing-output-" + rand.Text(), Forced: true,
	}
	admitted, accepted, err := service.Admit(ctx, job)
	if err != nil || !accepted {
		t.Fatal("The real GitHub service did not admit the routing output regression.")
	}
	outcome, _ := service.RunWithOutcome(ctx, admitted)
	after := liveReviewBodyCapture(t, ctx, client, opts, repository)
	if err := writeReviewBodyBackup(filepath.Join(directory, "after.json"), after); err != nil {
		t.Fatal("The interrupted fixture snapshot could not be saved privately.")
	}
	if outcome.Disposition != domain.AssessmentFailed && outcome.Disposition != domain.AssessmentIncomplete {
		t.Fatal("The interrupted model response did not retain an unsuccessful assessment.")
	}
	if err := logFile.Sync(); err != nil {
		t.Fatal("The private service log could not be synchronized.")
	}
	liveRoutingAssertTruncation(t, logPath, provider.Model)
	if after.PullRequest != before.PullRequest || !reflect.DeepEqual(after.Reviews, before.Reviews) || !reflect.DeepEqual(after.Threads, before.Threads) || len(after.Comments) != len(before.Comments) {
		t.Fatal("The interrupted review changed fixture metadata, verdicts, threads, or the summary count.")
	}
	body := livePartialSummary(t, ctx, client, opts, original.ID)
	stored, valid := marker.DecodeState(body)
	if !valid || stored.RunID != job.DeliveryID || stored.Status == marker.StateDone || stored.LastReviewed != "" || len(stored.Pending)+len(stored.Unread) == 0 || outcome.CoverageComplete {
		t.Fatal("The interrupted review incorrectly completed or discarded unread work.")
	}
	persisted, retainedFindings, _ := liveRoutingMetadata(t, body)
	var retainedExplanation string
	if json.Unmarshal(retainedFindings[0]["body"], &retainedExplanation) != nil || retainedExplanation != explanation || !strings.Contains(body, explanation) {
		t.Fatal("The interrupted review erased or shortened the saved metadata explanation.")
	}
	if string(persisted["head"]) != string(record["head"]) || string(persisted["revision"]) != string(record["revision"]) {
		t.Fatal("The interrupted review changed the saved metadata context.")
	}
	t.Logf("The real token interruption preserved the published explanation and incomplete checkpoint. Private evidence: %s", directory)
}

func liveRoutingMetadata(t *testing.T, body string) (map[string]json.RawMessage, []map[string]json.RawMessage, string) {
	t.Helper()
	const prefix = "<!-- pr-review-agent:metadata:v1 "
	start := strings.LastIndex(body, prefix)
	if start < 0 {
		t.Fatal("The summary requires a persisted metadata record.")
	}
	payload, _, found := strings.Cut(body[start+len(prefix):], " -->")
	if !found {
		t.Fatal("The persisted metadata record is incomplete.")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal("The persisted metadata record could not be decoded.")
	}
	var record map[string]json.RawMessage
	if json.Unmarshal(decoded, &record) != nil {
		t.Fatal("The persisted metadata record contains invalid JSON.")
	}
	var findings []map[string]json.RawMessage
	if json.Unmarshal(record["findings"], &findings) != nil || len(findings) == 0 {
		t.Fatal("The persisted metadata record requires an existing finding.")
	}
	return record, findings, prefix + payload + " -->"
}

func liveRoutingAssertTruncation(t *testing.T, path, model string) {
	t.Helper()
	file, err := os.Open(path)
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
		var record struct {
			Error string `json:"err"`
		}
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal("The private service log contains an invalid structured record.")
		}
		if strings.Contains(record.Error, (&openai.TruncatedError{Model: model}).Error()) {
			return
		}
	}
	t.Fatal("The service log does not establish an actual model token interruption.")
}

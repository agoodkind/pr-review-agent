//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/clock"
	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/reconcile"
	"goodkind.io/pr-review-agent/internal/review"
)

type liveFailureFixture struct {
	reference      domain.PullRequestRef
	expectedReview int64
}

type liveFailureProviderRecord struct {
	ProviderID      string `json:"provider_id"`
	ConfiguredModel string `json:"configured_model"`
	APIStatus       int    `json:"api_status"`
	Cause           review.ProviderFailureCause `json:"cause"`
}

func TestLiveUnexpectedFailureDismissesLatestOwnRejection(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_FAILURE_VERDICT_TEST") != "1" {
		t.Skip("The live failure verdict regression is not enabled.")
	}
	fixture := liveFailureTarget(t, "PR_AGENT_LIVE_PULL_REQUEST", "PR_AGENT_LIVE_FAILURE_REVIEW_ID")
	liveFailureRun(t, fixture, "CHANGES_REQUESTED")
}

func TestLiveUnexpectedFailurePreservesLaterOwnApproval(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_FAILURE_VERDICT_TEST") != "1" {
		t.Skip("The live failure verdict regression is not enabled.")
	}
	if os.Getenv("PR_AGENT_LIVE_FAILURE_APPROVAL_PULL_REQUEST") == "" {
		t.Skip("An explicitly selected approval fixture is not configured.")
	}
	fixture := liveFailureTarget(t, "PR_AGENT_LIVE_FAILURE_APPROVAL_PULL_REQUEST", "PR_AGENT_LIVE_FAILURE_APPROVAL_REVIEW_ID")
	liveFailureRun(t, fixture, "APPROVED")
}

func liveFailureTarget(t *testing.T, numberName, reviewName string) liveFailureFixture {
	t.Helper()
	parts := strings.Split(os.Getenv("PR_AGENT_LIVE_REPOSITORY"), "/")
	number, numberErr := strconv.Atoi(os.Getenv(numberName))
	installation, installationErr := strconv.ParseInt(os.Getenv("PR_AGENT_LIVE_INSTALLATION_ID"), 10, 64)
	expected, reviewErr := strconv.ParseInt(os.Getenv(reviewName), 10, 64)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || numberErr != nil || number <= 0 || installationErr != nil || installation <= 0 || reviewErr != nil || expected <= 0 {
		t.Fatal("The live regression requires explicit repository, pull request, installation, and expected review identifiers.")
	}
	var fixture liveFailureFixture
	fixture.reference = domain.PullRequestRef{Repository: domain.Repository{Owner: parts[0], Name: parts[1]}, InstallationID: installation, Number: number, Head: ""}
	fixture.expectedReview = expected
	return fixture
}

func liveFailureConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	keyPath := os.Getenv("PR_AGENT_LIVE_GITHUB_APP_KEY_FILE")
	providerID := os.Getenv("PR_AGENT_LIVE_FAILURE_PROVIDER_ID")
	if keyPath == "" || providerID == "" {
		t.Fatal("The live regression requires a GitHub app private-key file and an explicit runtime provider ID.")
	}
	key, err := cloudflareops.ReadCredential(keyPath)
	if err != nil {
		t.Fatal("The GitHub app private-key file could not be read.")
	}
	runtimePath := filepath.Join("..", "..", "runtime.json")
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal("The public runtime configuration could not be read.")
	}
	var runtime map[string]json.RawMessage
	if json.Unmarshal(data, &runtime) != nil {
		t.Fatal("The public runtime configuration could not be decoded.")
	}
	liveFailureProvider(t, runtime, providerID)
	for _, name := range []string{"REVIEW_RULES_FILE", "REVIEW_PROMPTS_FILE"} {
		var reference string
		if json.Unmarshal(runtime[name], &reference) != nil || reference == "" {
			t.Fatal("The runtime policy references are incomplete.")
		}
		if !filepath.IsAbs(reference) {
			reference = filepath.Join(filepath.Dir(runtimePath), reference)
		}
		runtime[name] = liveFailureEncode(t, reference)
	}
	encoded := liveFailureEncode(t, runtime)
	invalidCredential := "live-auth-error-" + rand.Text()
	webhookSecret := rand.Text()
	cfg, err := config.LoadRuntime(encoded, func(name string) (string, bool) {
		if name == "GITHUB_PRIVATE_KEY" {
			return string(key), true
		}
		if name == "GITHUB_WEBHOOK_SECRET" {
			return webhookSecret, true
		}
		if strings.HasSuffix(name, "_API_KEY") {
			return invalidCredential, true
		}
		return "", false
	})
	if err != nil {
		t.Fatal("The live GitHub identity and selected provider could not be configured.")
	}
	return cfg, invalidCredential
}

func liveFailureProvider(t *testing.T, runtime map[string]json.RawMessage, providerID string) {
	t.Helper()
	var definitions []map[string]json.RawMessage
	if json.Unmarshal(runtime["PROVIDERS"], &definitions) != nil {
		t.Fatal("The runtime providers could not be decoded.")
	}
	var selected map[string]json.RawMessage
	for _, definition := range definitions {
		var id string
		if json.Unmarshal(definition["id"], &id) != nil {
			t.Fatal("A runtime provider has no identifier.")
		}
		if id == providerID {
			if selected != nil {
				t.Fatal("The selected runtime provider is duplicated.")
			}
			selected = definition
		}
	}
	if selected == nil {
		t.Fatal("The provider identifier does not match the public runtime.")
	}
	for _, name := range []string{"cf_access_client_id_binding", "cf_access_client_secret_binding"} {
		var binding string
		if value, present := selected[name]; present && json.Unmarshal(value, &binding) == nil && binding != "" {
			t.Fatal("Select a provider that does not require Cloudflare Access credentials.")
		}
	}
	// The test removes app admission limits to exercise the provider authentication error.
	for _, name := range []string{"daily_token_limit", "daily_token_types", "token_limit", "token_types", "token_window", "token_limits"} {
		delete(selected, name)
	}
	selected["disabled"] = json.RawMessage("false")
	runtime["PROVIDERS"] = liveFailureEncode(t, []map[string]json.RawMessage{selected})
	runtime["PROVIDER_PRIORITY"] = liveFailureEncode(t, []string{providerID})
}

type liveFailureJSON interface {
	string | map[string]json.RawMessage | []map[string]json.RawMessage | []string
}

func liveFailureEncode[T liveFailureJSON](t *testing.T, value T) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal("The live regression configuration could not be encoded.")
	}
	return data
}

func liveFailureService(cfg config.Config, logger *slog.Logger) (*review.Service, *githubapp.Client) {
	githubHTTP := &http.Client{Transport: nil, CheckRedirect: nil, Jar: nil, Timeout: 30 * time.Second}
	github := githubapp.NewClient(cfg, githubHTTP, clock.System, logger)
	model := openai.NewClient(cfg, http.DefaultClient)
	reconciler := reconcile.NewService(github, model, cfg.GitHubBotLogin, logger, cfg.PromptBytes(), cfg.ReviewPolicy)
	service := review.NewService(github, diff.NewCollector(github), model, reconciler, queue.NewKeyedLocker(), cfg.GitHubBotLogin, cfg.MinimumImportance, cfg.ReviewMaxFiles, cfg.ReviewMaxChunks, cfg.ReviewChunkTimeout, cfg.ServiceFailureAppearance, clock.System, logger, cfg.ChunkConcurrency(), cfg.PromptBytes(), cfg.ReviewPolicy)
	service.SetFailureVerdictPolicy(config.DismissLatestFailureVerdict)
	return service, github
}

func liveFailureRun(t *testing.T, fixture liveFailureFixture, expectedState string) {
	t.Helper()
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
	logger := slog.New(slog.NewJSONHandler(logFile, nil))
	service, github := liveFailureService(cfg, logger)
	reference := fixture.reference
	original, err := github.GetPullRequest(ctx, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil || original.State != "open" || original.Draft || !original.EligibilityKnown || !original.MetadataKnown {
		t.Fatal("The explicitly selected fixture must have validated open, non-draft metadata.")
	}
	reference.Head = original.Head
	before, err := github.ListReviews(ctx, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil {
		t.Fatal("The fixture's current reviews could not be read with the GitHub app.")
	}
	latest := liveFailureLatestOwn(before, cfg.GitHubBotLogin)
	if latest.ID != fixture.expectedReview || latest.State != expectedState {
		t.Fatal("The latest own opinionated review differs from the explicitly selected fixture.")
	}
	if expectedState == "CHANGES_REQUESTED" && !slices.ContainsFunc(before, func(item githubapp.Review) bool {
		return item.Author != cfg.GitHubBotLogin && (item.State == "APPROVED" || item.State == "CHANGES_REQUESTED")
	}) {
		t.Fatal("The rejection fixture requires another author's submitted decision to verify ownership protection.")
	}
	if expectedState == "APPROVED" && !slices.ContainsFunc(before, func(item githubapp.Review) bool {
		return item.Author == cfg.GitHubBotLogin && item.State == "CHANGES_REQUESTED" && item.ID != latest.ID
	}) {
		t.Fatal("The approval fixture requires an earlier own requested-changes review.")
	}
	var job domain.ReviewJob
	job.PullRequestRef, job.DeliveryID, job.Forced = reference, "live-failure-verdict-"+rand.Text(), true
	job, admitted, err := service.Admit(ctx, job)
	if err != nil || !admitted {
		t.Fatal("The real GitHub app did not admit the explicit failure regression.")
	}
	outcome, _ := service.RunWithOutcome(ctx, job)
	if err := logFile.Sync(); err != nil {
		t.Fatal("The private service log could not be synchronized.")
	}
	liveFailureAssertProvider(t, logPath, cfg.Providers[0].ID, cfg.Providers[0].Model)
	if outcome.Disposition != domain.AssessmentFailed && outcome.Disposition != domain.AssessmentIncomplete {
		t.Fatal("The provider failure did not retain an unsuccessful assessment outcome.")
	}
	if !slices.Contains(outcome.FailureClasses, string(config.FailureOther)) || outcome.Nonce == "" || outcome.CoverageComplete {
		t.Fatal("The durable outcome did not retain the unexpected failure classification.")
	}
	after, err := github.ListReviews(ctx, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil {
		t.Fatal("The fixture's review states could not be read after the failure.")
	}
	liveFailureAssertReviews(t, before, after, latest, expectedState)
	liveFailureAssertReceipt(t, ctx, github, job, outcome, invalidCredential, cfg.GitHubBotLogin, cfg.Providers[0].ID)
	current, err := github.GetPullRequest(ctx, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil || !reflect.DeepEqual(current, original) {
		t.Fatal("The fixture metadata changed during the live regression.")
	}
	t.Logf("Live failure regression retained check %d and verified review %d state transitions.", job.CheckRunID, fixture.expectedReview)
}

func liveFailureLatestOwn(items []githubapp.Review, bot string) githubapp.Review {
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		if item.Author == bot && (item.State == "CHANGES_REQUESTED" || item.State == "APPROVED" || item.State == "DISMISSED") {
			return item
		}
	}
	var empty githubapp.Review
	return empty
}

func liveFailureAssertProvider(t *testing.T, logPath, providerID, model string) {
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
			break
		}
		if err != nil {
			t.Fatal("The private service log contains an invalid structured record.")
		}
		if record.ProviderID == providerID && record.ConfiguredModel == model && record.Cause == review.ProviderRequestFailed && record.APIStatus >= 400 && record.APIStatus < 500 && record.APIStatus != http.StatusTooManyRequests {
			return
		}
	}
	t.Fatal("The production service log does not establish an unexpected real provider authentication or request failure.")
}

func liveFailureAssertReviews(t *testing.T, before, after []githubapp.Review, latest githubapp.Review, expectedState string) {
	t.Helper()
	for _, old := range before {
		state := old.State
		if old.ID == latest.ID && expectedState == "CHANGES_REQUESTED" {
			state = "DISMISSED"
		}
		if !slices.ContainsFunc(after, func(item githubapp.Review) bool {
			return item.ID == old.ID && item.Author == old.Author && item.State == state
		}) {
			t.Fatalf("Review %d did not retain its required author and state.", old.ID)
		}
	}
	for _, item := range after {
		if item.Author != latest.Author || (item.State != "APPROVED" && item.State != "CHANGES_REQUESTED") {
			continue
		}
		if !slices.ContainsFunc(before, func(old githubapp.Review) bool { return old.ID == item.ID }) {
			t.Fatal("The failed assessment published a new bot verdict.")
		}
	}
}

func liveFailureAssertReceipt(t *testing.T, ctx context.Context, github *githubapp.Client, job domain.ReviewJob, outcome domain.AssessmentOutcome, invalidCredential, bot, providerID string) {
	t.Helper()
	check, found, err := github.FindCheckRunByExternalID(ctx, job.InstallationID, job.Repository, job.Head, config.ReviewCheckName, job.DeliveryID)
	if err != nil || !found || check.Status != "completed" || check.Outcome.Nonce != outcome.Nonce || check.Outcome.Disposition != outcome.Disposition || check.Outcome.CheckRunID != job.CheckRunID {
		t.Fatal("GitHub did not retain the failed assessment's durable outcome marker.")
	}
	comments, err := github.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		t.Fatal("The failure summary could not be read.")
	}
	for _, comment := range comments {
		state, valid := marker.DecodeState(comment.Body)
		if comment.Author != bot || !valid || state.RunID != job.DeliveryID || !strings.Contains(comment.Body, marker.Summary()) {
			continue
		}
		if strings.Contains(comment.Body, invalidCredential) || !strings.Contains(comment.Body, providerID) {
			t.Fatal("The public failure summary does not contain a safe operational failure explanation.")
		}
		return
	}
	t.Fatal("The failed assessment has no matching durable failure summary.")
}

//go:build integration

package quota_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/clock"
	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/reconcile"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestLiveCancelledReviewRetainsRecordedPeriodUsage(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_PERIOD_FAILURE_TEST") != "1" {
		t.Skip("The period failure regression requires an explicit live fixture.")
	}
	endpoint, server := accountingWorkerTLS(t, 0)
	cfg := loadUncappedRuntime(t, endpoint)
	key := livePeriodPrivateKey(t)
	cfg.GitHubPrivateKey = key
	cfg.Providers = cfg.Providers[:1]
	client := livePeriodHTTPClient(t, server)
	model := openai.NewClient(cfg, client)
	github := githubapp.NewClient(cfg, http.DefaultClient, clock.System, slog.Default())
	reference := livePeriodReference(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	pullRequest, err := github.GetPullRequest(ctx, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil || pullRequest.State != "closed" || !pullRequest.Draft || !strings.Contains(strings.ToLower(pullRequest.Title), "fixture") {
		t.Fatal("Select an explicit closed draft fixture for the period failure regression.")
	}
	reference.Head = pullRequest.Head
	original, comments := livePeriodSummary(t, ctx, github, reference, cfg.GitHubBotLogin)
	backup := backupLivePeriodSummary(t, original)
	t.Cleanup(func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), time.Minute)
		defer restoreCancel()
		if _, err := github.UpdateIssueComment(restoreCtx, reference.InstallationID, reference.Repository, original.ID, original.Body); err != nil {
			t.Error("Restore the original fixture summary:", err)
		}
	})
	prepared := removeLivePeriodDetails(t, original.Body)
	if _, err := github.UpdateIssueComment(ctx, reference.InstallationID, reference.Repository, original.ID, prepared); err != nil {
		t.Fatal("The test could not prepare the fixture summary.")
	}
	provider := cfg.Providers[0]
	storage := quota.NewClient(endpoint, cfg.GitHubWebhookSecret, client)
	reportDisplayUsage(t, storage, time.Now().UTC(), provider.ID, provider.Model, provider.Model, 20, 0, 0, 2, 22)
	ctx, _ = review.WithUsageRecorder(ctx)
	_, providerErr := model.Review(ctx, "Review this identity function: func identity(value int) int { return value }")
	var refused *openai.ProviderError
	if !errors.As(providerErr, &refused) || refused.StatusCode != http.StatusUnauthorized {
		t.Fatal("The real provider did not reject the invalid test credential with HTTP 401.")
	}
	service := review.NewService(github, diff.NewCollector(github), model,
		reconcile.NewService(github, model, cfg.GitHubBotLogin, slog.Default(), cfg.PromptBytes(), cfg.ReviewPolicy),
		queue.NewKeyedLocker(), cfg.GitHubBotLogin, cfg.MinimumImportance, cfg.ReviewMaxFiles,
		cfg.ReviewMaxChunks, cfg.ReviewChunkTimeout, cfg.ServiceFailureAppearance,
		clock.System, slog.Default(), cfg.ChunkConcurrency(), cfg.PromptBytes(), cfg.ReviewPolicy)
	job := domain.ReviewJob{PullRequestRef: reference, DeliveryID: "live-period-failure-" + rand.Text(), Forced: true}
	job, admitted, err := service.Admit(ctx, job)
	if err != nil || !admitted {
		t.Fatal("GitHub did not admit the period failure check.")
	}
	cancelled, cancelReview := context.WithCancel(ctx)
	cancelReview()
	if err := service.Reject(cancelled, job, providerErr); err == nil {
		t.Fatal("The cancelled review must retain its provider error.")
	}
	check, err := github.GetCheckRunDetails(ctx, reference.InstallationID, reference.Repository, job.CheckRunID)
	if err != nil || check.Run.Status != "completed" || check.Run.Conclusion != "cancelled" || !strings.Contains(check.Summary, "Recorded usage<br>22 tokens /") {
		t.Fatal("The cancelled check did not display its independently stored usage.")
	}
	current, after := livePeriodSummary(t, ctx, github, reference, cfg.GitHubBotLogin)
	if len(after) != len(comments) || current.ID != original.ID || !strings.Contains(current.Body, "Recorded usage<br>22 tokens /") {
		t.Fatal("The failure must update the existing summary with stored period totals.")
	}
	assertLivePeriodCheckpoint(t, original.Body, current.Body)
	t.Logf("Cancelled check %d and summary %d retained 22 stored tokens after caller cancellation. Original summary: %s", job.CheckRunID, original.ID, backup)
}

func backupLivePeriodSummary(t *testing.T, original githubapp.IssueComment) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "pragent-period-failure-live-")
	if err != nil {
		t.Fatal("The test could not create its private evidence directory.")
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal("The test could not encode the original fixture summary.")
	}
	path := filepath.Join(directory, "original-summary.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal("The test could not save the original fixture summary.")
	}
	t.Logf("Private fixture recovery file: %s", path)
	return path
}

func assertLivePeriodCheckpoint(t *testing.T, before, after string) {
	t.Helper()
	previous, previousValid := marker.DecodeState(before)
	current, currentValid := marker.DecodeState(after)
	if !previousValid || !currentValid || previous.LastReviewed != current.LastReviewed || !reflect.DeepEqual(previous.Completed, current.Completed) || !reflect.DeepEqual(previous.Pending, current.Pending) || !reflect.DeepEqual(previous.Unread, current.Unread) {
		t.Fatal("The failure changed the saved review coverage.")
	}
}

func livePeriodPrivateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	data, err := cloudflareops.ReadCredential(os.Getenv("PR_AGENT_LIVE_GITHUB_APP_KEY_FILE"))
	if err != nil {
		t.Fatal("The test could not read the explicit GitHub app key file.")
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("The GitHub app key must contain PEM data.")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal("The GitHub app key must contain a valid RSA private key.")
	}
	rsaKey, valid := key.(*rsa.PrivateKey)
	if !valid {
		t.Fatal("The GitHub app key must use RSA.")
	}
	return rsaKey
}

func livePeriodReference(t *testing.T) domain.PullRequestRef {
	t.Helper()
	parts := strings.Split(os.Getenv("PR_AGENT_LIVE_REPOSITORY"), "/")
	installation, installationErr := strconv.ParseInt(os.Getenv("PR_AGENT_LIVE_INSTALLATION_ID"), 10, 64)
	number, numberErr := strconv.Atoi(os.Getenv("PR_AGENT_LIVE_PULL_REQUEST"))
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || installationErr != nil || installation <= 0 || numberErr != nil || number <= 0 {
		t.Fatal("The live regression requires an explicit repository, installation, and pull request.")
	}
	return domain.PullRequestRef{Repository: domain.Repository{Owner: parts[0], Name: parts[1]}, InstallationID: installation, Number: number}
}

func livePeriodSummary(t *testing.T, ctx context.Context, github *githubapp.Client, reference domain.PullRequestRef, bot string) (githubapp.IssueComment, []githubapp.IssueComment) {
	t.Helper()
	comments, err := github.ListIssueComments(ctx, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil {
		t.Fatal("The test could not read the live fixture comments.")
	}
	var found githubapp.IssueComment
	for _, comment := range comments {
		if comment.Author == bot && marker.HasState(comment.Body) {
			if found.ID != 0 {
				t.Fatal("The fixture contains multiple bot summaries.")
			}
			found = comment
		}
	}
	if found.ID == 0 {
		t.Fatal("The live fixture requires an existing bot summary.")
	}
	return found, comments
}

func removeLivePeriodDetails(t *testing.T, body string) string {
	t.Helper()
	before, remaining, found := strings.Cut(body, "<details>\n<summary>Review details</summary>")
	if !found {
		t.Fatal("The fixture summary requires review details before preparation.")
	}
	_, after, found := strings.Cut(remaining, "</details>")
	if !found {
		t.Fatal("The fixture review details are incomplete.")
	}
	return before + after
}

func livePeriodHTTPClient(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	roots, err := x509.SystemCertPool()
	if err != nil {
		t.Fatal("The test could not load the system certificate pool.")
	}
	roots.AddCert(server.Certificate())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: time.Minute}
}

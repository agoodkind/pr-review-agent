package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/queue"
)

func TestFullQueueReturns503AndReleasesClaim(t *testing.T) {
	releaseJob := make(chan struct{})
	cfg := config.Config{
		GitHubWebhookSecret: []byte("test-webhook-secret"), // gitleaks:allow
	}
	cache := queue.NewDeliveryCache(100, time.Hour, time.Now)
	dispatcher := queue.NewDispatcher(1, 1, blockingRunner{release: releaseJob}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := newHandler(
		cfg,
		cache,
		dispatcher,
		passThroughAdmitter{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	server := httptest.NewServer(handler)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher.Start(ctx)

	body := []byte(`{"action":"opened","installation":{"id":1},"repository":{"name":"repo","owner":{"login":"owner"}},"pull_request":{"number":1,"draft":false,"head":{"sha":"a3c4f1cac7f595bc824704b9d2a1f1191630dc32"}}}`)
	post := func(deliveryID string) *http.Response {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/github_webhooks", stringsReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		request.Header.Set("X-Github-Event", "pull_request")
		request.Header.Set("X-Github-Delivery", deliveryID)
		request.Header.Set("X-Hub-Signature-256", signTestBody(body))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("POST webhook: %v", err)
		}
		return response
	}

	first := post("delivery-blocked-1")
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.StatusCode)
	}
	_ = first.Body.Close()

	fill := post("delivery-blocked-2")
	if fill.StatusCode != http.StatusAccepted {
		t.Fatalf("fill status = %d, want 202", fill.StatusCode)
	}
	_ = fill.Body.Close()

	third := post("delivery-blocked-3")
	if third.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third status = %d, want 503", third.StatusCode)
	}
	_ = third.Body.Close()

	close(releaseJob)
	time.Sleep(200 * time.Millisecond)

	retry := post("delivery-blocked-3")
	if retry.StatusCode != http.StatusAccepted {
		t.Fatalf("retry status = %d, want 202 after claim release", retry.StatusCode)
	}
	_ = retry.Body.Close()
}

type blockingRunner struct {
	release chan struct{}
}

type passThroughAdmitter struct{}

func (passThroughAdmitter) Admit(_ context.Context, job domain.ReviewJob) (domain.ReviewJob, bool, error) {
	job.CheckRunID = 1
	job.CheckRunStatus = "in_progress"
	return job, true, nil
}

func (passThroughAdmitter) Reject(context.Context, domain.ReviewJob, error) error {
	return nil
}

func (runner blockingRunner) Run(context.Context, domain.ReviewJob) error {
	<-runner.release
	return nil
}

func stringsReader(body []byte) *strings.Reader {
	return strings.NewReader(string(body))
}

func signTestBody(body []byte) string {
	mac := hmac.New(sha256.New, []byte("test-webhook-secret")) // gitleaks:allow
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

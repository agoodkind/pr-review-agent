package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/app"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/telemetry"
	"goodkind.io/pr-review-agent/internal/version"
)

func TestDrainClosesAdmissionThroughRunningApplication(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	key := []byte("drain-test-signing-material")
	application := app.New(config.Config{
		Port: strconv.Itoa(port), ReviewWorkers: 1, GitHubWebhookSecret: key,
		ClydeBaseURL: &url.URL{Scheme: "https", Host: "provider.example"},
	}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	application.Start(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := application.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get(origin + "/health")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("initial health status = %d", response.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, operation := range []struct {
		body   string
		signed bool
		code   int
		closed bool
	}{
		{`{"action":"begin"}`, false, http.StatusUnauthorized, false},
		{`{"action":"status"}`, true, http.StatusOK, false},
		{`{"action":"invalid"}`, true, http.StatusBadRequest, false},
		{`{"action":"idle"}`, true, http.StatusOK, true},
		{`{"action":"begin"}`, true, http.StatusOK, true},
		{`{"action":"begin"}`, true, http.StatusOK, true},
		{`{"action":"status"}`, true, http.StatusOK, true},
	} {
		body := []byte(operation.body)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/internal/v1/drain", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if operation.signed {
			request.Header.Set("X-Pr-Agent-Drain-Signature", telemetry.Sign(key, body))
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != operation.code {
			t.Fatalf("drain %s status = %d, want %d", body, response.StatusCode, operation.code)
		}
		if operation.code == http.StatusOK {
			var status queue.Status
			if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
				t.Fatal(err)
			}
			if status.AdmissionClosed != operation.closed || status.Outstanding != 0 || status.Running != 0 || status.Queued != 0 {
				t.Fatalf("unexpected drain status: %+v", status)
			}
		}
		_ = response.Body.Close()
	}
	for _, path := range []string{"/", "/health"} {
		response, err := http.Get(origin + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("X-Pr-Agent-Revision") != version.Commit {
			t.Fatalf("drained health = %d revision %q", response.StatusCode, response.Header.Get("X-Pr-Agent-Revision"))
		}
	}
}

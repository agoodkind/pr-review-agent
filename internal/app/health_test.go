package app_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/app"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/version"
)

func TestHealthProbeChecksRunningServiceRevision(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	application := app.New(config.Config{
		Port: strconv.Itoa(port), ReviewWorkers: 1,
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
	revision := version.SourceRevision()
	for _, expected := range []string{"", revision, revision + "0", "ffffffffffffffffffffffffffffffffffffffff"} {
		command := exec.CommandContext(t.Context(), "../../deploy/cloudflare/scripts/probe-health.sh")
		command.Env = append(os.Environ(), "HEALTH_URL="+origin+"/", "EXPECTED_REVISION="+expected,
			"MAX_ATTEMPTS=10", "RETRY_DELAY_SECONDS=0.1")
		output, err := command.CombinedOutput()
		if expected != "" && expected != revision {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("probe accepted a different service revision: %v: %s", err, output)
			}
		} else if err != nil {
			t.Fatalf("probe refused the running service revision: %v: %s", err, output)
		}
	}
	for _, path := range []string{"/", "/health"} {
		response, err := http.Get(origin + path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		revisions, present := response.Header[http.CanonicalHeaderKey("X-Pr-Agent-Revision")]
		if response.StatusCode != http.StatusOK || !present || len(revisions) != 1 || revisions[0] != version.SourceRevision() {
			t.Fatalf("health response did not identify the running build: status=%d revision=%q", response.StatusCode, revisions)
		}
	}
}

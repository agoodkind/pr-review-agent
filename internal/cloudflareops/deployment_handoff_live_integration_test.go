//go:build integration

package cloudflareops

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestGatewayHandoffConflictRetriesNative(t *testing.T) {
	origin := os.Getenv("PRAGENT_HANDOFF_TEST_ORIGIN")
	if origin == "" {
		t.Skip("requires the running native service routing fixture")
	}
	revision := os.Getenv("PRAGENT_HANDOFF_TEST_REVISION")
	if !revisionPattern.MatchString(revision) {
		t.Fatal("native handoff test requires the actual service revision")
	}
	token, err := ReadCredential(os.Getenv("PRAGENT_HANDOFF_TEST_TOKEN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(origin, string(token), nil)
	if err != nil {
		t.Fatal(err)
	}
	operation := &deployment{
		client: client,
		configuration: DeploymentConfiguration{
			GatewayOrigin: origin, CandidateTimeout: "5s", ProbeInterval: "100ms",
		},
		healthClient: &http.Client{Timeout: time.Second},
	}
	ctx := t.Context()
	assertNativeSourceRunning(t, operation, ctx, revision)
	_, err = operation.gatewayHandoffControl(ctx, "advance", revision)
	if !errors.Is(err, errHandoffControlUnavailable) || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("advance before configured handoff must report retryable HTTP 409: %v", err)
	}
	waitContext, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	err = operation.waitHandoffControl(waitContext, "advance", revision, true)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unconfigured handoff must wait until its deadline: %v", err)
	}
	if !errors.Is(err, errHandoffControlUnavailable) || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("handoff timeout must preserve its retryable HTTP status: %v", err)
	}
	assertNativeSourceRunning(t, operation, ctx, revision)
}

func assertNativeSourceRunning(t *testing.T, operation *deployment, ctx context.Context, revision string) {
	t.Helper()
	status, err := operation.gatewayHandoffControl(ctx, "status", revision)
	if err != nil {
		t.Fatal(err)
	}
	if *status.Handoff || *status.Complete || !*status.PreviousContainerRunning {
		t.Fatalf("unconfigured handoff must preserve the running source: %+v", status)
	}
	if _, err := operation.probe(ctx, operation.configuration.GatewayOrigin, revision); err != nil {
		t.Fatalf("source must continue serving its actual revision: %v", err)
	}
}

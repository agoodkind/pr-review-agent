//go:build integration

package cloudflareops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGatewayHandoffConflictRetriesNative(t *testing.T) {
	operation, revision := nativeHandoffDeployment(t)
	ctx := t.Context()
	assertNativeSourceRunning(t, operation, ctx, revision)
	_, err := operation.gatewayHandoffControl(ctx, "advance", revision)
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

func TestGatewayHandoffReadinessRetriesNative(t *testing.T) {
	operation, revision := nativeHandoffDeployment(t)
	signalPath := os.Getenv("PRAGENT_HANDOFF_TEST_RECOVERY_SIGNAL_FILE")
	if signalPath == "" {
		t.Fatal("native handoff recovery requires a private synchronization file path")
	}
	ctx := t.Context()
	assertNativeUnreadyHandoff(t, operation, ctx)
	waitContext, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	err := operation.waitHandoffControl(waitContext, "advance", revision, true)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unready handoff must wait until its deadline after HTTP 503: %v", err)
	}
	var readinessError *readinessHTTPError
	if !errors.As(err, &readinessError) || readinessError.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readiness timeout must preserve the actual HTTP status: %v", err)
	}
	assertNativeUnreadyHandoff(t, operation, ctx)
	t.Log("Persistent readiness HTTP 503 preserved the running source until the deadline.")
	recoveryContext, cancelRecovery := context.WithCancelCause(ctx)
	defer cancelRecovery(nil)
	var observed sync.Once
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() {
		observed.Do(func() {
			if err := os.WriteFile(signalPath+".tmp", []byte("response_received\n"), 0o600); err != nil {
				cancelRecovery(fmt.Errorf("write native handoff recovery receipt: %w", err))
				return
			}
			if err := os.Rename(signalPath+".tmp", signalPath); err != nil {
				cancelRecovery(fmt.Errorf("publish native handoff recovery receipt: %w", err))
			}
		})
	}}
	recoveryContext = httptrace.WithClientTrace(recoveryContext, trace)
	if err := operation.waitHandoffControl(recoveryContext, "status", revision, true); err != nil {
		t.Fatalf("the same waiter must recover after the actual candidate revision becomes available: %v", errors.Join(err, context.Cause(recoveryContext)))
	}
	status, err := operation.gatewayHandoffControl(ctx, "status", revision)
	if err != nil {
		t.Fatal(err)
	}
	if !*status.Complete || !*status.CandidateReady || *status.PreviousContainerRunning || status.CandidateRevision != revision {
		t.Fatalf("recovered handoff must prove the exact candidate revision and stopped source: %+v", status)
	}
	if _, err := operation.probe(ctx, operation.configuration.GatewayOrigin, revision); err != nil {
		t.Fatalf("recovered gateway must serve the exact candidate revision: %v", err)
	}
	t.Log("The recovery waiter completed with the exact candidate revision and stopped source.")
}

func nativeHandoffDeployment(t *testing.T) (*deployment, string) {
	t.Helper()
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
			GatewayOrigin: origin, CandidateTimeout: "2m", ProbeInterval: "100ms",
		},
		healthClient: &http.Client{Timeout: time.Second},
		directory:    t.TempDir(),
	}
	return operation, revision
}

func assertNativeUnreadyHandoff(t *testing.T, operation *deployment, ctx context.Context) {
	t.Helper()
	status, err := operation.gatewayHandoffControl(ctx, "status", strings.Repeat("0", 40))
	if err != nil {
		t.Fatal(err)
	}
	if !*status.Handoff || *status.Complete || *status.CandidateReady || !*status.PreviousContainerRunning {
		t.Fatalf("unready handoff must preserve the running source: %+v", status)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, operation.configuration.GatewayOrigin+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := operation.healthClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("actual unready candidate must return HTTP 503, got %d", response.StatusCode)
	}
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

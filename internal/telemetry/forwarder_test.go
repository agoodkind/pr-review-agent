package telemetry_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/telemetry"
)

// testSigningKey stands in for the shared webhook signing key in these tests.
const testSigningKey = "fixture-hmac-material"

// The service's logs reach no readable sink from inside the container. This
// proves an ordinary log call arrives at the receiver, signed, with its fields.
func TestALoggedLineReachesTheReceiver(t *testing.T) {
	received := newBatchRecorder()
	server := httptest.NewServer(received)
	t.Cleanup(server.Close)

	forwarder := telemetry.NewForwarder(server.URL, []byte(testSigningKey), nil)
	if forwarder == nil {
		t.Fatal("NewForwarder returned nil for a configured endpoint")
	}
	logger := slog.New(forwarder)

	logger.Info("review job started", slog.Int("pull_request", 282))

	if err := forwarder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	batches := received.batches()
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(batches))
	}
	if len(batches[0].Records) != 1 {
		t.Fatalf("records = %d, want 1", len(batches[0].Records))
	}
	record := batches[0].Records[0]
	if record.Message != "review job started" {
		t.Fatalf("message = %q, want the logged message", record.Message)
	}
	if record.Fields["pull_request"] != "282" {
		t.Fatalf("fields = %v, want the logged attribute", record.Fields)
	}
	if !received.signatureValid() {
		t.Fatal("batch signature did not verify with the shared secret")
	}
}

// A receiver that is unreachable must not stop the service from logging or
// reviewing, so a failed shipment is discarded rather than retried forever.
func TestAnUnreachableReceiverDoesNotBlockLogging(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	forwarder := telemetry.NewForwarder(server.URL, []byte(testSigningKey), nil)
	logger := slog.New(forwarder)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			logger.Info("noise")
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging blocked on an unreachable receiver")
	}
	if err := forwarder.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// Without a destination the service still starts and logs to stdout only.
func TestNoDestinationReturnsNoForwarder(t *testing.T) {
	if telemetry.NewForwarder("", []byte(testSigningKey), nil) != nil {
		t.Fatal("NewForwarder returned a forwarder without an endpoint")
	}
	if telemetry.NewForwarder("https://example.test/logs", nil, nil) != nil {
		t.Fatal("NewForwarder returned a forwarder without a secret")
	}
}

type batchRecorder struct {
	mu       sync.Mutex
	captured []telemetry.Batch
	valid    bool
}

func newBatchRecorder() *batchRecorder {
	return &batchRecorder{mu: sync.Mutex{}, captured: nil, valid: false}
}

func (recorder *batchRecorder) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	var batch telemetry.Batch
	if err := json.Unmarshal(body, &batch); err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.captured = append(recorder.captured, batch)
	recorder.valid = request.Header.Get(telemetry.SignatureHeader) == telemetry.Sign([]byte(testSigningKey), body)
	writer.WriteHeader(http.StatusOK)
}

func (recorder *batchRecorder) batches() []telemetry.Batch {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	copied := make([]telemetry.Batch, len(recorder.captured))
	copy(copied, recorder.captured)
	return copied
}

func (recorder *batchRecorder) signatureValid() bool {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.valid
}

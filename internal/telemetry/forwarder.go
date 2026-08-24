// Package telemetry ships the service's own logs somewhere a person can read
// them.
//
// The service runs inside a Cloudflare container. Container stdout reaches no
// log sink: seven days of the deployed Worker's logs contain 6413 events and
// not one of them came from this service. Every log the service wrote was
// therefore unreadable, which is why production failures could only be guessed
// at from what reached a pull request.
//
// This forwarder closes that gap at the logging layer rather than at each call
// site. It is an [slog.Handler], so every log the service already writes is
// shipped, and new logs are shipped without touching this package.
package telemetry

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	// SignatureHeader carries the signature of the forwarded batch.
	SignatureHeader = "X-Pr-Agent-Signature-256"
	// bufferCapacity bounds retained records. Logging must never block a review,
	// so the oldest record is dropped once the buffer is full.
	bufferCapacity = 512
	// batchLimit bounds one request body.
	batchLimit = 64
	// flushInterval is how often buffered records are shipped.
	flushInterval = 5 * time.Second
	// shipTimeout bounds one shipping request.
	shipTimeout = 10 * time.Second
)

// Record is one forwarded log line.
type Record struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// Batch is one signed group of forwarded log lines.
type Batch struct {
	Records []Record `json:"records"`
}

// Forwarder ships log records to an endpoint that writes them to a readable log
// sink. It never blocks the caller and never fails a review.
type Forwarder struct {
	endpoint   string
	signingKey []byte
	httpClient *http.Client
	attributes []slog.Attr

	mu      *sync.Mutex
	pending *[]Record

	stop   chan struct{}
	closed *sync.Once
	done   chan struct{}
}

// NewForwarder constructs a forwarder and starts its flush loop. It returns nil
// when forwarding is not configured, and the caller then runs without it rather
// than failing to start.
func NewForwarder(endpoint string, signingKey []byte, httpClient *http.Client) *Forwarder {
	if endpoint == "" || len(signingKey) == 0 {
		return nil
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: shipTimeout}
	}
	pending := make([]Record, 0, bufferCapacity)
	forwarder := &Forwarder{
		endpoint:   endpoint,
		signingKey: signingKey,
		httpClient: httpClient,
		attributes: nil,
		mu:         &sync.Mutex{},
		pending:    &pending,
		stop:       make(chan struct{}),
		closed:     &sync.Once{},
		done:       make(chan struct{}),
	}
	go func() {
		// A panic in the flush loop must not end the service. Log shipping is
		// the least important thing this process does.
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error(
					"log forwarder panicked",
					slog.Any("panic", recovered),
					slog.String("err", "log forwarder panicked"),
				)
			}
		}()
		forwarder.run()
	}()
	return forwarder
}

func (forwarder *Forwarder) run() {
	defer close(forwarder.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			forwarder.flush()
		case <-forwarder.stop:
			forwarder.flush()
			return
		}
	}
}

// Enabled records every level, because a forwarder that drops levels hides the
// detail a failure needs.
func (forwarder *Forwarder) Enabled(context.Context, slog.Level) bool {
	return forwarder != nil
}

// Handle buffers one record. It drops the oldest record rather than blocking,
// because shipping a log must never delay a review.
func (forwarder *Forwarder) Handle(_ context.Context, record slog.Record) error {
	if forwarder == nil {
		return nil
	}
	fields := make(map[string]string, record.NumAttrs()+len(forwarder.attributes))
	for _, attribute := range forwarder.attributes {
		fields[attribute.Key] = attribute.Value.String()
	}
	record.Attrs(func(attribute slog.Attr) bool {
		fields[attribute.Key] = attribute.Value.String()
		return true
	})
	entry := Record{
		Time:    record.Time,
		Level:   record.Level.String(),
		Message: record.Message,
		Fields:  fields,
	}

	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	buffered := *forwarder.pending
	if len(buffered) >= bufferCapacity {
		buffered = buffered[1:]
	}
	buffered = append(buffered, entry)
	*forwarder.pending = buffered
	return nil
}

// WithAttrs returns a forwarder that adds the given attributes to every record
// and shares the same buffer.
func (forwarder *Forwarder) WithAttrs(attrs []slog.Attr) slog.Handler {
	if forwarder == nil {
		return nil
	}
	combined := make([]slog.Attr, 0, len(forwarder.attributes)+len(attrs))
	combined = append(combined, forwarder.attributes...)
	combined = append(combined, attrs...)
	return &Forwarder{
		endpoint:   forwarder.endpoint,
		signingKey: forwarder.signingKey,
		httpClient: forwarder.httpClient,
		attributes: combined,
		mu:         forwarder.mu,
		pending:    forwarder.pending,
		stop:       forwarder.stop,
		closed:     forwarder.closed,
		done:       forwarder.done,
	}
}

// WithGroup returns the forwarder unchanged, because forwarded records are flat.
func (forwarder *Forwarder) WithGroup(string) slog.Handler {
	return forwarder
}

// Close flushes what is buffered and stops the flush loop.
func (forwarder *Forwarder) Close() error {
	if forwarder == nil {
		return nil
	}
	forwarder.closed.Do(func() {
		close(forwarder.stop)
	})
	<-forwarder.done
	return nil
}

func (forwarder *Forwarder) flush() {
	for {
		batch := forwarder.take()
		if len(batch.Records) == 0 {
			return
		}
		forwarder.send(batch)
	}
}

func (forwarder *Forwarder) take() Batch {
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	buffered := *forwarder.pending
	if len(buffered) == 0 {
		return Batch{Records: nil}
	}
	size := min(len(buffered), batchLimit)
	records := make([]Record, size)
	copy(records, buffered[:size])
	*forwarder.pending = buffered[size:]
	return Batch{Records: records}
}

// send ships one batch. A shipping failure is discarded rather than retried,
// because a log that cannot be shipped must not grow without bound or stall a
// review, and reporting the failure through slog would recurse into this
// handler.
func (forwarder *Forwarder) send(batch Batch) {
	body, err := json.Marshal(batch)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shipTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, forwarder.endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(SignatureHeader, Sign(forwarder.signingKey, body))

	response, err := forwarder.httpClient.Do(request)
	if err != nil {
		return
	}
	_ = response.Body.Close()
}

// Sign returns the signature header value for one body. It matches the scheme
// GitHub uses for webhooks, so the receiver verifies forwarded logs the same
// way it verifies a delivery and no extra credential is needed.
func Sign(signingKey []byte, body []byte) string {
	mac := hmac.New(sha256.New, signingKey)
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

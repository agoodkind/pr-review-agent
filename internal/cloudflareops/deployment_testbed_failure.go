package cloudflareops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"goodkind.io/pr-review-agent/internal/clock"
	"goodkind.io/pr-review-agent/internal/version"
)

const (
	testFailureCaptureTimeout = 30 * time.Second
	testFailureRequestTimeout = 3 * time.Second
	testFailureProbeInterval  = 2 * time.Second
)

type testFailureReadinessRecord struct {
	Timestamp          time.Time `json:"timestamp"`
	Kind               string    `json:"kind"`
	ControllerRevision string    `json:"controller_revision,omitempty"`
	Worker             string    `json:"worker,omitempty"`
	Origin             string    `json:"origin,omitempty"`
	Exists             *bool     `json:"exists,omitempty"`
	Enabled            *bool     `json:"enabled,omitempty"`
	PreviewsEnabled    *bool     `json:"previews_enabled,omitempty"`
	Status             int       `json:"status,omitempty"`
	Revision           string    `json:"revision,omitempty"`
	Server             string    `json:"server,omitempty"`
	CFRay              string    `json:"cf_ray,omitempty"`
	Error              string    `json:"error,omitempty"`
}

func captureTestFailureReadiness(ctx context.Context, operation *deployment, originalError error) (returnErr error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), testFailureCaptureTimeout)
	defer cancel()
	file, err := os.OpenFile(filepath.Join(operation.directory, "failure-readiness.ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		slog.WarnContext(ctx, "Failure readiness evidence creation failed")
		return fmt.Errorf("create failure readiness evidence: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	encoder := json.NewEncoder(file)
	var failure testFailureReadinessRecord
	failure.Timestamp, failure.Kind, failure.Error = clock.System().UTC(), "failure", originalError.Error()
	if err := encoder.Encode(failure); err != nil {
		slog.WarnContext(ctx, "Original failure readiness evidence write failed", "path", file.Name())
		return fmt.Errorf("write readiness %s record to %s: %w", failure.Kind, file.Name(), err)
	}
	var identity testFailureReadinessRecord
	identity.Timestamp, identity.Kind, identity.ControllerRevision = clock.System().UTC(), "controller", version.SourceRevision()
	if err := encoder.Encode(identity); err != nil {
		slog.WarnContext(ctx, "Controller readiness evidence write failed", "path", file.Name())
		return fmt.Errorf("write readiness %s record to %s: %w", identity.Kind, file.Name(), err)
	}
	targets := append([]DeploymentSlot{{Name: operation.name, Origin: operation.configuration.GatewayOrigin}}, operation.configuration.Slots...)
	records := make(chan testFailureReadinessRecord, len(targets))
	var workers sync.WaitGroup
	for _, target := range targets {
		workers.Go(func() { observeTestFailureWorker(ctx, operation, target, records) })
	}
	observerResult := make(chan error, 1)
	go func() {
		defer func() {
			var observerError error
			if recovered := recover(); recovered != nil {
				observerError = errors.New("failure readiness observer panicked")
				slog.ErrorContext(ctx, "Failure readiness observer panicked", "err", observerError)
				cancel()
			}
			close(records)
			observerResult <- observerError
		}()
		workers.Wait()
	}()
	for record := range records {
		if returnErr != nil {
			continue
		}
		if err := encoder.Encode(record); err != nil {
			slog.WarnContext(ctx, "Failure readiness evidence write failed", "path", file.Name(), "kind", record.Kind)
			returnErr = fmt.Errorf("write readiness %s record to %s: %w", record.Kind, file.Name(), err)
			cancel()
		}
	}
	return errors.Join(returnErr, <-observerResult)
}

func observeTestFailureWorker(ctx context.Context, operation *deployment, target DeploymentSlot, records chan<- testFailureReadinessRecord) {
	var presence testFailureReadinessRecord
	presence.Timestamp, presence.Kind, presence.Worker, presence.Origin = clock.System().UTC(), "presence", target.Name, target.Origin
	requestContext, cancel := context.WithTimeout(ctx, testFailureRequestTimeout)
	_, exists, err := operation.client.workerSettings(requestContext, operation.account, target.Name, true)
	cancel()
	if err != nil {
		presence.Error = err.Error()
	} else {
		presence.Exists = &exists
	}
	records <- presence
	if err != nil || !exists {
		return
	}
	records <- readTestFailureSubdomain(ctx, operation, target)
	ticker := time.NewTicker(testFailureProbeInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		records <- readTestFailureOrigin(ctx, operation, target)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func readTestFailureSubdomain(ctx context.Context, operation *deployment, target DeploymentSlot) testFailureReadinessRecord {
	var record testFailureReadinessRecord
	record.Timestamp, record.Kind, record.Worker, record.Origin = clock.System().UTC(), "subdomain", target.Name, target.Origin
	ctx, cancel := context.WithTimeout(ctx, testFailureRequestTimeout)
	defer cancel()
	response, status, err := operation.client.request(ctx, http.MethodGet, "/accounts/"+operation.account+"/workers/scripts/"+target.Name+"/subdomain", nil)
	record.Status = status
	if err != nil {
		record.Error = err.Error()
		return record
	}
	var state struct {
		Enabled         *bool `json:"enabled"`
		PreviewsEnabled *bool `json:"previews_enabled"`
	}
	if json.Unmarshal(response.Result, &state) != nil || state.Enabled == nil || state.PreviewsEnabled == nil {
		record.Error = "worker subdomain response did not provide enabled and previews_enabled booleans"
		return record
	}
	record.Enabled, record.PreviewsEnabled = state.Enabled, state.PreviewsEnabled
	return record
}

func readTestFailureOrigin(ctx context.Context, operation *deployment, target DeploymentSlot) testFailureReadinessRecord {
	var record testFailureReadinessRecord
	record.Timestamp, record.Kind, record.Worker, record.Origin = clock.System().UTC(), "readiness", target.Name, target.Origin
	ctx, cancel := context.WithTimeout(ctx, testFailureRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.Origin+"/", nil)
	if err != nil {
		record.Error = "failure readiness request could not be created"
		return record
	}
	response, err := operation.healthClient.Do(request)
	if err != nil {
		record.Error = err.Error()
		return record
	}
	defer func() { _ = response.Body.Close() }()
	record.Status, record.Revision = response.StatusCode, response.Header.Get("X-Pr-Agent-Revision")
	record.Server, record.CFRay = response.Header.Get("Server"), response.Header.Get("Cf-Ray")
	return record
}

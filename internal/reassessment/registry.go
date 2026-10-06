package reassessment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/telemetry"
)

// Observation records validated live values without treating missing data as completion.
type Observation struct {
	EligibilityKnown bool                         `json:"eligibility_known"`
	State            string                       `json:"state"`
	Merged           bool                         `json:"merged"`
	Draft            bool                         `json:"draft"`
	Head             domain.HeadSHA               `json:"head"`
	MetadataRevision string                       `json:"metadata_revision"`
	CheckFound       bool                         `json:"check_found"`
	CheckID          int64                        `json:"check_id"`
	CheckStatus      string                       `json:"check_status"`
	Disposition      domain.AssessmentDisposition `json:"disposition"`
	OutcomeNonce     string                       `json:"outcome_nonce"`
	SummaryFound     bool                         `json:"summary_found"`
	SummaryStatus    string                       `json:"summary_status"`
	LastReviewed     domain.HeadSHA               `json:"last_reviewed"`
	Pending          int                          `json:"pending"`
	Unread           int                          `json:"unread"`
	FailureStage     string                       `json:"failure_stage"`
}

// Target survives queue cleanup and retains the service's own assessment evidence.
type Target struct {
	ObservedAtMS        int64                    `json:"observed_at_ms"`
	Failures            int                      `json:"failures"`
	Key                 string                   `json:"key"`
	Version             int64                    `json:"version"`
	Job                 domain.ReviewJob         `json:"job"`
	NotBeforeMS         int64                    `json:"not_before_ms"`
	PrivateOutcome      domain.AssessmentOutcome `json:"private_outcome"`
	PrivateNonce        string                   `json:"private_nonce"`
	PrivateDeliveryID   string                   `json:"private_delivery_id"`
	PrivateGeneration   string                   `json:"private_generation"`
	PrivateObservedAtMS int64                    `json:"private_observed_at_ms"`
	Observation         Observation              `json:"observation"`
	Confirmations       int                      `json:"confirmations"`
}

// Reconciliation commits its observed target and queue repair in one transaction.
type Reconciliation struct {
	QueueVersionKnown     bool    `json:"queue_version_known"`
	Key                   string  `json:"key"`
	ExpectedTargetVersion int64   `json:"expected_target_version"`
	Target                Target  `json:"target"`
	ExpectedQueueVersion  int64   `json:"expected_queue_version"`
	Record                *Record `json:"record"`
	RepairKind            string  `json:"repair_kind"`
}

type registryMutation struct {
	ExpectedQueueVersion    int64        `json:"expected_queue_version"`
	ExpectedQueueGeneration string       `json:"expected_queue_generation"`
	Action                  string       `json:"action"`
	Key                     string       `json:"key"`
	ExpectedVersion         int64        `json:"expected_version"`
	Target                  *Target      `json:"target,omitempty"`
	Sweep                   *SweepCursor `json:"sweep,omitempty"`
}

type registryResponse struct {
	Applied bool         `json:"applied"`
	Target  *Target      `json:"target"`
	Sweep   *SweepCursor `json:"sweep"`
}

func (client *Client) registryRequest(ctx context.Context, mutation registryMutation) (registryResponse, error) {
	var result registryResponse
	logger := gklog.L(ctx)
	body, err := json.Marshal(mutation)
	if err != nil {
		logger.WarnContext(ctx, "encode target registry request", slog.String("err", err.Error()))
		return result, fmt.Errorf("encode target registry request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.URL, bytes.NewReader(body))
	if err != nil {
		logger.WarnContext(ctx, "create target registry request", slog.String("err", err.Error()))
		return result, fmt.Errorf("create target registry request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(SignatureHeader, telemetry.Sign(client.SigningKey, body))
	response, err := client.HTTP.Do(request)
	if err != nil {
		logger.WarnContext(ctx, "read target registry", slog.String("err", err.Error()))
		return result, fmt.Errorf("read target registry: %w", err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			logger.WarnContext(ctx, "close target registry response", slog.String("err", closeErr.Error()))
		}
	}()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("target registry returned HTTP %d", response.StatusCode)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if readErr != nil {
		logger.WarnContext(ctx, "read target registry response", slog.String("err", readErr.Error()))
		return result, fmt.Errorf("read target registry response: %w", readErr)
	}
	var fields map[string]json.RawMessage
	if decodeErr := json.Unmarshal(data, &fields); decodeErr != nil || fields == nil || len(fields["target"]) == 0 {
		return result, fmt.Errorf("target registry response is incomplete")
	}
	if err := json.Unmarshal(data, &result); err != nil {
		logger.WarnContext(ctx, "decode target registry response", slog.String("err", err.Error()))
		return result, fmt.Errorf("decode target registry response: %w", err)
	}
	return result, nil
}

// NewTarget schedules independent reconciliation without replacing private outcomes.
func (coordinator *Coordinator) NewTarget(job domain.ReviewJob, now time.Time) Target {
	var target Target
	target.Key = coordinator.key(job)
	target.Job = job
	interval, _ := time.ParseDuration(coordinator.Settings.ReconcileInterval)
	target.NotBeforeMS = now.Add(interval).UnixMilli()
	return target
}

// Track preserves discovery evidence before a webhook can settle.
func (coordinator *Coordinator) Track(ctx context.Context, job domain.ReviewJob) error {
	if !coordinator.Settings.Enabled {
		return nil
	}
	return coordinator.Store.RegisterTarget(ctx, coordinator.NewTarget(job, coordinator.Clock()))
}

func (coordinator *Coordinator) recordPrivateOutcome(ctx context.Context, job domain.ReviewJob, outcome domain.AssessmentOutcome, prior *Record) error {
	key := coordinator.key(job)
	target, err := coordinator.Store.ReadTarget(ctx, key)
	if err != nil {
		return err
	}
	var updated Target
	version := int64(0)
	if target == nil {
		updated = coordinator.NewTarget(job, coordinator.Clock())
	} else {
		updated = *target
		version = target.Version
	}
	updated.PrivateOutcome = outcome
	updated.PrivateNonce = outcome.Nonce
	updated.PrivateDeliveryID = job.DeliveryID
	updated.PrivateGeneration = job.ReassessmentGeneration
	updated.PrivateObservedAtMS = coordinator.Clock().UnixMilli()
	queueVersion := int64(0)
	queueGeneration := ""
	if prior != nil {
		queueVersion = prior.Version
		queueGeneration = prior.Generation
	}
	applied, err := coordinator.Store.WriteTarget(ctx, updated, version, queueVersion, queueGeneration)
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("private assessment registry changed before persistence")
	}
	return nil
}

// ReadTarget requires a successful structured registry response before claiming absence.
func (client *Client) ReadTarget(ctx context.Context, key string) (*Target, error) {
	response, err := client.registryRequest(ctx, registryMutation{ExpectedQueueVersion: 0, ExpectedQueueGeneration: "", Action: "query_target", Key: key, ExpectedVersion: 0, Target: nil, Sweep: nil})
	return response.Target, err
}

// RegisterTarget preserves private outcomes when inventory or webhook hints recur.
func (client *Client) RegisterTarget(ctx context.Context, target Target) error {
	_, err := client.registryRequest(ctx, registryMutation{ExpectedQueueVersion: 0, ExpectedQueueGeneration: "", Action: "register_target", Key: target.Key, ExpectedVersion: 0, Target: &target, Sweep: nil})
	return err
}

// WriteTarget rejects an outcome update based on an obsolete observation.
func (client *Client) WriteTarget(ctx context.Context, target Target, expectedVersion int64, expectedQueueVersion int64, expectedQueueGeneration string) (bool, error) {
	response, err := client.registryRequest(ctx, registryMutation{ExpectedQueueVersion: expectedQueueVersion, ExpectedQueueGeneration: expectedQueueGeneration, Action: "write_target", Key: target.Key, ExpectedVersion: expectedVersion, Target: &target, Sweep: nil})
	return response.Applied, err
}

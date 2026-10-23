package cloudflareops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

var (
	errDeploymentCredentialInactive = errors.New("candidate deployment credential is not active (HTTP 401)")
	errDeploymentControlUnpublished = errors.New("candidate deployment control is not published (HTTP 404)")
	errDeploymentControlUnavailable = errors.New("candidate deployment control is unavailable (HTTP 503)")
)

type drainStatusWire struct {
	ContainerRunning *bool `json:"container_running"`
	AdmissionClosed  *bool `json:"admission_closed"`
	Queued           *int  `json:"queued"`
	Running          *int  `json:"running"`
	Outstanding      *int  `json:"outstanding"`
}

type drainState interface {
	containerRunning() bool
	drained() bool
}

type stoppedDrainState struct{}

func (stoppedDrainState) containerRunning() bool { return false }
func (stoppedDrainState) drained() bool          { return true }

type runningDrainState struct {
	admissionClosed bool
	queued          int
	running         int
	outstanding     int
}

func (runningDrainState) containerRunning() bool { return true }
func (state runningDrainState) drained() bool {
	return state.admissionClosed && state.outstanding == 0
}

func (operation *deployment) registerDeploymentToken(ctx context.Context, slot DeploymentSlot) error {
	if err := operation.writeDeploymentToken(ctx, slot.Name); err != nil {
		return err
	}
	_, err := operation.waitDrainControl(ctx, slot.Origin, "status")
	return err
}

func (operation *deployment) writeDeploymentToken(ctx context.Context, script string) error {
	digest := sha256.Sum256([]byte(operation.client.token))
	body, err := json.Marshal(struct {
		Name string `json:"name"`
		Text string `json:"text"`
		Type string `json:"type"`
	}{Name: "DEPLOYMENT_TOKEN_SHA256", Text: hex.EncodeToString(digest[:]), Type: "secret_text"})
	if err != nil {
		return errors.New("deployment token digest could not be encoded")
	}
	response, _, err := operation.client.request(ctx, http.MethodPut, "/accounts/"+operation.account+"/workers/scripts/"+script+"/secrets", body)
	if err != nil {
		return err
	}
	var binding struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if json.Unmarshal(response.Result, &binding) != nil || binding.Name != "DEPLOYMENT_TOKEN_SHA256" || binding.Type != "secret_text" {
		return errors.New("cloudflare did not acknowledge the deployment digest binding")
	}
	return nil
}

func (operation *deployment) waitDrainControl(ctx context.Context, origin, action string) (drainState, error) {
	timeout, interval := operation.configuration.candidateTimeout, operation.configuration.probeInterval
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastError error
	for {
		state, err := operation.drainRequest(ctx, origin, action)
		if err == nil {
			return state, nil
		}
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "Candidate drain control exceeded its elapsed limit", "action", action)
			return nil, fmt.Errorf("candidate drain %s did not become available before its timeout: %w", action, errors.Join(lastError, err, context.Cause(ctx)))
		}
		if !errors.Is(err, errDeploymentCredentialInactive) && !errors.Is(err, errDeploymentControlUnpublished) && !errors.Is(err, errDeploymentControlUnavailable) {
			return nil, fmt.Errorf("candidate drain %s failed: %w", action, err)
		}
		lastError = err
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			slog.WarnContext(ctx, "Candidate drain control exceeded its elapsed limit", "action", action)
			return nil, fmt.Errorf("candidate drain %s did not become available before its timeout: %w", action, errors.Join(lastError, context.Cause(ctx)))
		case <-timer.C:
		}
	}
}

func (operation *deployment) drainRequest(ctx context.Context, origin, action string) (drainState, error) {
	var status drainStatusWire
	body, err := json.Marshal(struct {
		Action string `json:"action"`
	}{Action: action})
	if err != nil {
		return nil, errors.New("drain action could not be encoded")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(origin, "/")+"/internal/v1/drain", bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("drain request could not be created")
	}
	request.Header.Set("Authorization", "Bearer "+operation.client.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := operation.healthClient.Do(request)
	if err != nil {
		slog.WarnContext(ctx, "Candidate drain transport failed")
		return nil, fmt.Errorf("candidate drain transport failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized {
		return nil, errDeploymentCredentialInactive
	}
	if response.StatusCode == http.StatusNotFound {
		return nil, errDeploymentControlUnpublished
	}
	if response.StatusCode == http.StatusServiceUnavailable {
		return nil, errDeploymentControlUnavailable
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("candidate drain returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&status); err != nil {
		return nil, errors.New("candidate drain returned invalid JSON")
	}
	if status.ContainerRunning == nil {
		return nil, errors.New("candidate drain omitted container_running")
	}
	if !*status.ContainerRunning {
		return stoppedDrainState{}, nil
	}
	if status.AdmissionClosed == nil || status.Queued == nil || status.Running == nil || status.Outstanding == nil {
		return nil, errors.New("running candidate drain omitted admission or work counts")
	}
	if *status.Queued < 0 || *status.Running < 0 || *status.Outstanding < 0 {
		return nil, errors.New("candidate drain returned negative work counts")
	}
	if *status.Outstanding < *status.Queued || *status.Outstanding-*status.Queued != *status.Running {
		return nil, errors.New("candidate drain outstanding count does not equal queued plus running")
	}
	return runningDrainState{admissionClosed: *status.AdmissionClosed, queued: *status.Queued, running: *status.Running, outstanding: *status.Outstanding}, nil
}

func (operation *deployment) beginDrain(ctx context.Context, slot DeploymentSlot) (drainState, error) {
	if err := operation.registerDeploymentToken(ctx, slot); err != nil {
		return nil, err
	}
	return operation.waitDrainControl(ctx, slot.Origin, "begin")
}

func (operation *deployment) waitDrained(ctx context.Context, slot DeploymentSlot) error {
	timeout, interval := operation.configuration.candidateTimeout, operation.configuration.probeInterval
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	status, err := operation.beginDrain(ctx, slot)
	if err != nil {
		return err
	}
	for !status.drained() {
		if err := operation.requireOriginalSelection(ctx); err != nil {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("inactive candidate still has outstanding work; refusing to replace its image")
		case <-timer.C:
		}
		if _, err := operation.probe(ctx, operation.configuration.GatewayOrigin, operation.activeRevision); err != nil {
			return err
		}
		status, err = operation.waitDrainControl(ctx, slot.Origin, "status")
		if err != nil {
			return err
		}
	}
	return nil
}

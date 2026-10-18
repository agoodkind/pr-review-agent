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

var errDeploymentCredentialInactive = errors.New("candidate deployment credential is not active (HTTP 401)")

type drainStatus struct {
	ContainerRunning *bool `json:"container_running"`
	AdmissionClosed  *bool `json:"admission_closed"`
	Queued           *int  `json:"queued"`
	Running          *int  `json:"running"`
	Outstanding      *int  `json:"outstanding"`
}

func (operation *deployment) registerDeploymentToken(ctx context.Context, slot DeploymentSlot) error {
	if err := operation.writeDeploymentToken(ctx, slot.Name); err != nil {
		return err
	}
	return operation.waitDeploymentCredential(ctx, slot)
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

func (operation *deployment) waitDeploymentCredential(ctx context.Context, slot DeploymentSlot) error {
	timeout, _ := time.ParseDuration(operation.configuration.CandidateTimeout)
	interval, _ := time.ParseDuration(operation.configuration.ProbeInterval)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		_, err := operation.drainRequest(ctx, slot.Origin, "status")
		if err == nil {
			return nil
		}
		if !errors.Is(err, errDeploymentCredentialInactive) {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errDeploymentCredentialInactive
		case <-timer.C:
		}
	}
}

func (operation *deployment) drainRequest(ctx context.Context, origin, action string) (drainStatus, error) {
	var status drainStatus
	body, err := json.Marshal(struct {
		Action string `json:"action"`
	}{Action: action})
	if err != nil {
		return status, errors.New("drain action could not be encoded")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(origin, "/")+"/internal/v1/drain", bytes.NewReader(body))
	if err != nil {
		return status, errors.New("drain request could not be created")
	}
	request.Header.Set("Authorization", "Bearer "+operation.client.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := operation.healthClient.Do(request)
	if err != nil {
		slog.WarnContext(ctx, "Candidate drain transport failed")
		return status, fmt.Errorf("candidate drain transport failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized {
		return status, errDeploymentCredentialInactive
	}
	if response.StatusCode != http.StatusOK {
		return status, fmt.Errorf("candidate drain returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&status); err != nil {
		return status, errors.New("candidate drain returned invalid JSON")
	}
	if status.ContainerRunning == nil {
		return status, errors.New("candidate drain omitted container_running")
	}
	if !*status.ContainerRunning {
		return status, nil
	}
	if status.AdmissionClosed == nil || status.Queued == nil || status.Running == nil || status.Outstanding == nil {
		return status, errors.New("running candidate drain omitted admission or work counts")
	}
	if *status.Queued < 0 || *status.Running < 0 || *status.Outstanding < 0 {
		return status, errors.New("candidate drain returned negative work counts")
	}
	if *status.Outstanding < *status.Queued || *status.Outstanding-*status.Queued != *status.Running {
		return status, errors.New("candidate drain outstanding count does not equal queued plus running")
	}
	return status, nil
}

func (operation *deployment) beginDrain(ctx context.Context, slot DeploymentSlot) (drainStatus, error) {
	if err := operation.registerDeploymentToken(ctx, slot); err != nil {
		return drainStatus{}, err
	}
	return operation.drainRequest(ctx, slot.Origin, "begin")
}

func drained(status drainStatus) bool {
	return !*status.ContainerRunning || (*status.AdmissionClosed && *status.Outstanding == 0)
}

func (operation *deployment) waitDrained(ctx context.Context, slot DeploymentSlot) error {
	timeout, _ := time.ParseDuration(operation.configuration.CandidateTimeout)
	interval, _ := time.ParseDuration(operation.configuration.ProbeInterval)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	status, err := operation.beginDrain(ctx, slot)
	if err != nil {
		return err
	}
	for !drained(status) {
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
		status, err = operation.drainRequest(ctx, slot.Origin, "status")
		if err != nil {
			return err
		}
	}
	return nil
}

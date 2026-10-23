package cloudflareops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

var errHandoffControlUnavailable = errors.New("gateway handoff control is not ready")

type handoffControlStatus struct {
	Handoff                  *bool  `json:"handoff"`
	CandidateReady           *bool  `json:"candidate_ready"`
	CandidateRevision        string `json:"candidate_revision"`
	PreviousContainerRunning *bool  `json:"previous_container_running"`
	Complete                 *bool  `json:"complete"`
}

func (status handoffControlStatus) MarshalJSON() ([]byte, error) {
	type wireStatus handoffControlStatus
	data, err := json.Marshal(wireStatus(status))
	if err != nil {
		return nil, fmt.Errorf("encode handoff control status: %w", err)
	}
	return data, nil
}

func setHandoffVariables(configuration wranglerConfiguration, enabled bool, revision string) error {
	variables := make(map[string]json.RawMessage)
	if json.Unmarshal(configuration["vars"], &variables) != nil || variables == nil {
		return errors.New("handoff gateway requires readable vars")
	}
	flag := "false"
	if enabled {
		flag = "true"
	}
	for name, value := range map[string]string{"REVIEW_HANDOFF": flag, "REVIEW_HANDOFF_REVISION": revision} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return errors.New("handoff variable could not be encoded")
		}
		variables[name] = encoded
	}
	return setWranglerValue(configuration, "vars", variables)
}

func configureGatewayHandoff(configuration wranglerConfiguration, active deploymentSelection, before workerSettings, handoff deploymentHandoff) error {
	if err := configureGateway(configuration, active, before); err != nil {
		return err
	}
	if err := setHandoffVariables(configuration, true, handoff.Revision); err != nil {
		return err
	}
	var services []serviceBinding
	if json.Unmarshal(configuration["services"], &services) != nil {
		return errors.New("handoff gateway requires readable service bindings")
	}
	services = append(services, serviceBinding{Name: "CANDIDATE_REVIEW_SERVICE", Service: handoff.Service, Entrypoint: "ReviewService", Environment: ""})
	return setWranglerValue(configuration, "services", services)
}

func (operation *deployment) startHandoff(ctx context.Context) error {
	if err := operation.writeDeploymentToken(ctx, operation.name); err != nil {
		return err
	}
	if err := operation.waitHandoffControl(ctx, "status", operation.options.Revision, false); err != nil {
		return err
	}
	handoff := deploymentHandoff{Enabled: true, Revision: operation.options.Revision, Service: operation.slot.Name}
	if err := configureGatewayHandoff(operation.gateway, operation.selection, operation.before, handoff); err != nil {
		return err
	}
	if err := operation.requireOriginalSelection(ctx); err != nil {
		return err
	}
	if err := operation.upload(ctx, "start-handoff", operation.options.Wrangler, operation.gateway, true); err != nil {
		return err
	}
	operation.handoff = handoff
	return operation.finishHandoff(ctx)
}

func (operation *deployment) resumeHandoff(ctx context.Context) error {
	slog.InfoContext(ctx, "Persisted handoff resume started", "candidate", operation.handoff.Service, "revision", operation.handoff.Revision)
	if err := operation.writeDeploymentToken(ctx, operation.name); err != nil {
		return err
	}
	if err := operation.waitHandoffControl(ctx, "status", operation.handoff.Revision, false); err != nil {
		return err
	}
	revision := operation.handoff.Revision
	if err := operation.finishHandoff(ctx); err != nil {
		return err
	}
	if revision != operation.options.Revision {
		return fmt.Errorf("existing handoff for %s completed; requested release %s has not been deployed; rerun deployment", revision, operation.options.Revision)
	}
	return nil
}

func (operation *deployment) finishHandoff(ctx context.Context) error {
	if err := operation.waitHandoffControl(ctx, "advance", operation.handoff.Revision, true); err != nil {
		return err
	}
	if err := operation.requireOriginalSelection(ctx); err != nil {
		return err
	}
	revision := operation.handoff.Revision
	selection := deploymentSelection{Mode: "service", Service: operation.handoff.Service}
	if err := configureGateway(operation.gateway, selection, operation.before); err != nil {
		return err
	}
	if err := setHandoffVariables(operation.gateway, false, ""); err != nil {
		return err
	}
	if err := operation.upload(ctx, "promote-candidate", operation.options.Wrangler, operation.gateway, true); err != nil {
		return err
	}
	settings, _, err := operation.client.workerSettings(ctx, operation.account, operation.name, false)
	if err != nil {
		return err
	}
	selected, err := settings.selection(operation.configuration)
	if err != nil {
		return err
	}
	handoff, err := settings.handoff(operation.configuration)
	if err != nil {
		return err
	}
	if selected != selection || handoff.Enabled {
		return errors.New("gateway did not finalize the verified candidate selection")
	}
	if err := preserveBindings(operation.before, settings); err != nil {
		return err
	}
	if _, err := operation.probe(ctx, operation.configuration.GatewayOrigin, revision); err != nil {
		return err
	}
	return nil
}

func (operation *deployment) gatewayHandoffControl(ctx context.Context, action, revision string) (handoffControlStatus, error) {
	var status handoffControlStatus
	body, err := json.Marshal(struct {
		Action           string `json:"action"`
		ExpectedRevision string `json:"expected_revision"`
	}{Action: action, ExpectedRevision: revision})
	if err != nil {
		return status, errors.New("handoff control request could not be encoded")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(operation.configuration.GatewayOrigin, "/")+"/internal/v1/deployment", bytes.NewReader(body))
	if err != nil {
		return status, errors.New("handoff control request could not be created")
	}
	request.Header.Set("Authorization", "Bearer "+operation.client.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := operation.healthClient.Do(request)
	if err != nil {
		slog.WarnContext(ctx, "Gateway handoff transport failed")
		return status, fmt.Errorf("gateway handoff transport failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized {
		return status, errDeploymentCredentialInactive
	}
	if response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusServiceUnavailable {
		return status, fmt.Errorf("%w (HTTP %d)", errHandoffControlUnavailable, response.StatusCode)
	}
	if response.StatusCode != http.StatusOK {
		return status, fmt.Errorf("gateway handoff control returned HTTP %d", response.StatusCode)
	}
	if json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&status) != nil {
		return status, errors.New("gateway handoff control returned invalid JSON")
	}
	if status.Handoff == nil || status.CandidateReady == nil || status.PreviousContainerRunning == nil || status.Complete == nil {
		return status, errors.New("gateway handoff control omitted required state")
	}
	if *status.Complete && (!*status.Handoff || !*status.CandidateReady || *status.PreviousContainerRunning || status.CandidateRevision != revision) {
		return status, errors.New("gateway handoff completion does not prove candidate readiness and stopped previous process")
	}
	return status, nil
}

func (operation *deployment) waitHandoffControl(ctx context.Context, action, revision string, complete bool) error {
	timeout, _ := time.ParseDuration(operation.configuration.CandidateTimeout)
	interval, _ := time.ParseDuration(operation.configuration.ProbeInterval)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var confirmed handoffControlStatus
	var lastError error
	for {
		_, err := operation.probe(ctx, operation.configuration.GatewayOrigin, "")
		var status handoffControlStatus
		if err == nil {
			status, err = operation.gatewayHandoffControl(ctx, action, revision)
		}
		if err == nil && (!complete || *status.Complete) {
			confirmed = status
			break
		}
		if ctx.Err() != nil {
			slog.WarnContext(ctx, "Gateway handoff exceeded its elapsed limit", "action", action)
			return fmt.Errorf("gateway handoff %s did not finish before its timeout; rerun deployment to resume safely: %w", action, errors.Join(lastError, err, ctx.Err()))
		}
		var readinessError *readinessHTTPError
		retryableReadiness := errors.As(err, &readinessError) && readinessError.StatusCode == http.StatusServiceUnavailable
		if err != nil && !retryableReadiness && !errors.Is(err, errDeploymentCredentialInactive) && !errors.Is(err, errHandoffControlUnavailable) {
			return err
		}
		lastError = err
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			slog.WarnContext(ctx, "Gateway handoff exceeded its elapsed limit", "action", action)
			return fmt.Errorf("gateway handoff %s did not finish before its timeout; rerun deployment to resume safely: %w", action, errors.Join(lastError, ctx.Err()))
		case <-timer.C:
		}
	}
	slog.InfoContext(ctx, "Gateway handoff control confirmed", "action", action, "complete", *confirmed.Complete)
	if complete {
		return WriteJSON(operation.directory+"/handoff-complete.json", confirmed)
	}
	return nil
}

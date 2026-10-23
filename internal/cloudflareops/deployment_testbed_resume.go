package cloudflareops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

type testHandoffObservation struct {
	handoff deploymentHandoff
	err     error
}

type testHandoffResumeReceipt struct {
	Candidate                   string `json:"candidate"`
	Revision                    string `json:"revision"`
	VersionBefore               string `json:"version_before"`
	VersionAfter                string `json:"version_after"`
	Interrupted                 bool   `json:"interrupted"`
	ResumedPromoted             bool   `json:"resumed_promoted"`
	ConfiguredUnavailableOrigin string `json:"configured_unavailable_origin"`
	ConfiguredPublicStatus      int    `json:"configured_public_status"`
	UnavailableWorker           string `json:"unavailable_worker"`
	UnavailableWorkerExists     bool   `json:"unavailable_worker_exists"`
	GatewayRevisionBeforeResume string `json:"gateway_revision_before_resume"`
}

func (receipt testHandoffResumeReceipt) MarshalJSON() ([]byte, error) {
	type wireReceipt testHandoffResumeReceipt
	data, err := json.Marshal(wireReceipt(receipt))
	if err != nil {
		return nil, fmt.Errorf("encode handoff resume receipt: %w", err)
	}
	return data, nil
}

func (operation *deployment) deployWithInterruptedHandoff(ctx context.Context, options DeploymentOptions) (DeploymentResult, error) {
	interrupted, cancel := context.WithCancel(ctx)
	defer cancel()
	observations := make(chan testHandoffObservation, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err := errors.New("persisted handoff observer panicked")
				slog.ErrorContext(interrupted, "Persisted handoff observer panicked", "err", err)
				observations <- testHandoffObservation{handoff: deploymentHandoff{Enabled: false, Revision: "", Service: ""}, err: err}
				cancel()
			}
		}()
		handoff, err := operation.observePersistedTestHandoff(interrupted)
		observations <- testHandoffObservation{handoff: handoff, err: err}
		cancel()
	}()
	result, deploymentErr := Deploy(interrupted, options)
	cancel()
	observation := <-observations
	if observation.err != nil {
		return result, observation.err
	}
	if deploymentErr == nil || !errors.Is(interrupted.Err(), context.Canceled) || result.Promoted {
		return result, errors.New("test deployment did not stop after its persisted handoff")
	}
	if observation.handoff.Revision != options.Revision || observation.handoff.Service != operation.configuration.Slots[1].Name {
		return result, errors.New("interrupted test handoff did not retain the expected candidate and revision")
	}
	_, before, err := operation.testWorkerVersionNamed(ctx, observation.handoff.Service)
	if err != nil {
		return result, err
	}
	configurationPath, readiness, err := prepareUnavailableTestResumeOrigin(ctx, operation, observation.handoff.Revision)
	if err != nil {
		return result, err
	}
	settings, _, err := operation.client.workerSettings(ctx, operation.account, operation.name, false)
	if err != nil {
		return result, err
	}
	retained, err := settings.handoff(operation.configuration)
	if err != nil {
		return result, err
	}
	if retained != observation.handoff {
		return result, errors.New("saved candidate service binding or revision changed before handoff resume")
	}
	receipt := readiness
	receipt.Candidate, receipt.Revision, receipt.VersionBefore, receipt.Interrupted = observation.handoff.Service, observation.handoff.Revision, before, true
	if err := WriteJSON(filepath.Join(operation.directory, "handoff-resume-readiness.json"), receipt); err != nil {
		return result, err
	}
	options.Directory += "-resumed"
	options.Configuration = configurationPath
	result, err = Deploy(ctx, options)
	if err != nil {
		return result, err
	}
	_, after, err := operation.testWorkerVersionNamed(ctx, observation.handoff.Service)
	if err != nil {
		return result, err
	}
	if before != after || !result.Promoted || result.PendingHandoffRevision != observation.handoff.Revision {
		return result, errors.New("resumed deployment replaced its candidate or did not finish its recorded handoff")
	}
	receipt.VersionAfter, receipt.ResumedPromoted = after, result.Promoted
	return result, WriteJSON(filepath.Join(operation.directory, "handoff-resume.json"), receipt)
}

func prepareUnavailableTestResumeOrigin(ctx context.Context, operation *deployment, revision string) (string, testHandoffResumeReceipt, error) {
	var readiness testHandoffResumeReceipt
	gateway, err := url.Parse(operation.configuration.GatewayOrigin)
	if err != nil {
		slog.WarnContext(ctx, "Isolated gateway origin parsing failed")
		return "", readiness, fmt.Errorf("parse isolated gateway origin: %w", err)
	}
	domain := strings.TrimPrefix(gateway.Hostname(), operation.name+".")
	readiness.UnavailableWorker = strings.TrimSuffix(operation.name, "-gateway") + "-unavailable"
	readiness.ConfiguredUnavailableOrigin = "https://" + readiness.UnavailableWorker + "." + domain
	_, exists, err := operation.client.workerSettings(ctx, operation.account, readiness.UnavailableWorker, true)
	if err != nil {
		return "", readiness, err
	}
	readiness.UnavailableWorkerExists = exists
	if exists {
		return "", readiness, errors.New("unavailable test origin already has a Worker; refusing to use it")
	}
	readiness.GatewayRevisionBeforeResume, err = operation.probe(ctx, operation.configuration.GatewayOrigin, revision)
	if err != nil {
		return "", readiness, err
	}
	_, err = operation.probe(ctx, readiness.ConfiguredUnavailableOrigin, "")
	var readinessError *readinessHTTPError
	if err == nil {
		return "", readiness, errors.New("unavailable test origin returned HTTP 200 instead of HTTP 404")
	}
	if !errors.As(err, &readinessError) || readinessError.StatusCode != http.StatusNotFound {
		slog.WarnContext(ctx, "Unavailable test origin did not return HTTP 404")
		return "", readiness, fmt.Errorf("unavailable test origin must return HTTP 404: %w", err)
	}
	readiness.ConfiguredPublicStatus = readinessError.StatusCode
	configuration := operation.configuration
	configuration.Slots = append([]DeploymentSlot(nil), operation.configuration.Slots...)
	configuration.Slots[1].Origin = readiness.ConfiguredUnavailableOrigin
	data, err := json.Marshal(configuration)
	if err != nil {
		slog.WarnContext(ctx, "Isolated resume configuration encoding failed")
		return "", readiness, fmt.Errorf("encode isolated resume configuration: %w", err)
	}
	configurationPath := filepath.Join(operation.directory, "unavailable-origin-deployment.json")
	if err := WriteJSON(configurationPath, json.RawMessage(data)); err != nil {
		return "", readiness, err
	}
	return configurationPath, readiness, nil
}

func (operation *deployment) observePersistedTestHandoff(ctx context.Context) (deploymentHandoff, error) {
	for {
		settings, _, err := operation.client.workerSettings(ctx, operation.account, operation.name, false)
		if err != nil {
			return deploymentHandoff{}, err
		}
		handoff, err := settings.handoff(operation.configuration)
		if err != nil {
			return deploymentHandoff{}, err
		}
		if handoff.Enabled {
			return handoff, nil
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return deploymentHandoff{}, errors.New("test controller did not observe a persisted handoff before deployment ended")
		case <-timer.C:
		}
	}
}

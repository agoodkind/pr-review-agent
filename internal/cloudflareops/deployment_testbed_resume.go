package cloudflareops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"
)

type testHandoffObservation struct {
	handoff deploymentHandoff
	err     error
}

type testHandoffResumeReceipt struct {
	Candidate                    string `json:"candidate"`
	Revision                     string `json:"revision"`
	VersionBefore                string `json:"version_before"`
	VersionAfter                 string `json:"version_after"`
	Interrupted                  bool   `json:"interrupted"`
	ResumedPromoted              bool   `json:"resumed_promoted"`
	CandidatePublicRouteDisabled bool   `json:"candidate_public_route_disabled"`
	CandidatePublicStatus        int    `json:"candidate_public_status"`
	GatewayRevisionBeforeResume  string `json:"gateway_revision_before_resume"`
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
	publicStatus, gatewayRevision, err := disableTestCandidatePublicRoute(ctx, operation, operation.configuration.Slots[1], observation.handoff.Revision)
	if err != nil {
		return result, err
	}
	receipt := testHandoffResumeReceipt{Candidate: observation.handoff.Service, Revision: observation.handoff.Revision, VersionBefore: before, VersionAfter: "", Interrupted: true, ResumedPromoted: false, CandidatePublicRouteDisabled: true, CandidatePublicStatus: publicStatus, GatewayRevisionBeforeResume: gatewayRevision}
	if err := WriteJSON(filepath.Join(operation.directory, "handoff-resume-readiness.json"), receipt); err != nil {
		return result, err
	}
	options.Directory += "-resumed"
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

func disableTestCandidatePublicRoute(ctx context.Context, operation *deployment, slot DeploymentSlot, revision string) (int, string, error) {
	body, err := json.Marshal(struct {
		Enabled         bool `json:"enabled"`
		PreviewsEnabled bool `json:"previews_enabled"`
	}{Enabled: false, PreviewsEnabled: false})
	if err != nil {
		slog.WarnContext(ctx, "Test candidate public route encoding failed", "candidate", slot.Name)
		return 0, "", fmt.Errorf("encode disabled test candidate public route: %w", err)
	}
	response, _, err := operation.client.request(ctx, http.MethodPost, "/accounts/"+operation.account+"/workers/scripts/"+slot.Name+"/subdomain", body)
	if err != nil {
		return 0, "", err
	}
	var state struct {
		Enabled         *bool `json:"enabled"`
		PreviewsEnabled *bool `json:"previews_enabled"`
	}
	if json.Unmarshal(response.Result, &state) != nil || state.Enabled == nil || state.PreviewsEnabled == nil || *state.Enabled || *state.PreviewsEnabled {
		return 0, "", errors.New("test candidate public and preview routes did not match the disabled configuration")
	}
	return waitTestCandidatePublicRouteDisabled(ctx, operation, slot, revision)
}

func waitTestCandidatePublicRouteDisabled(ctx context.Context, operation *deployment, slot DeploymentSlot, revision string) (int, string, error) {
	timeout, interval := operation.configuration.candidateTimeout, operation.configuration.probeInterval
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		gatewayRevision, err := operation.probe(ctx, operation.configuration.GatewayOrigin, revision)
		if err != nil {
			slog.WarnContext(ctx, "Gateway readiness failed during test candidate public route removal", "candidate", slot.Name)
			return 0, "", fmt.Errorf("gateway readiness failed while disabling the test candidate public route: %w", err)
		}
		_, err = operation.probe(ctx, slot.Origin, "")
		var readinessError *readinessHTTPError
		if errors.As(err, &readinessError) && readinessError.StatusCode == http.StatusNotFound {
			return readinessError.StatusCode, gatewayRevision, nil
		}
		if err != nil {
			slog.WarnContext(ctx, "Disabled test candidate public route returned an unexpected response", "candidate", slot.Name)
			return 0, "", fmt.Errorf("disabled test candidate public route returned an unexpected response: %w", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			slog.WarnContext(ctx, "Test candidate public route removal exceeded its elapsed limit", "candidate", slot.Name)
			return 0, "", fmt.Errorf("disabled test candidate public route did not return HTTP 404 before its timeout: %w", ctx.Err())
		case <-timer.C:
		}
	}
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

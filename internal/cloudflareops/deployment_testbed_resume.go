package cloudflareops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"
)

type testHandoffObservation struct {
	handoff deploymentHandoff
	err     error
}

type testHandoffResumeReceipt struct {
	Candidate       string `json:"candidate"`
	Revision        string `json:"revision"`
	VersionBefore   string `json:"version_before"`
	VersionAfter    string `json:"version_after"`
	Interrupted     bool   `json:"interrupted"`
	ResumedPromoted bool   `json:"resumed_promoted"`
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
	receipt := testHandoffResumeReceipt{Candidate: observation.handoff.Service, Revision: observation.handoff.Revision, VersionBefore: before, VersionAfter: after, Interrupted: true, ResumedPromoted: result.Promoted}
	return result, WriteJSON(filepath.Join(operation.directory, "handoff-resume.json"), receipt)
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

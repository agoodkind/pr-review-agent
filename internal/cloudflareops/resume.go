package cloudflareops

import (
	"context"
	"errors"
	"path/filepath"
)

var (
	errSavedHandoffChanged = errors.New(
		"saved candidate service binding or revision changed before handoff resume",
	)
	errResumedHandoffChanged = errors.New(
		"resumed deployment replaced its candidate or did not finish its recorded handoff",
	)
)

func (operation *deployment) resumeTestHandoff(
	ctx context.Context,
	options DeploymentOptions,
	result DeploymentResult,
	handoff deploymentHandoff,
) (DeploymentResult, error) {
	_, before, err := operation.testWorkerVersionNamed(ctx, handoff.Service)
	if err != nil {
		return result, err
	}

	configurationPath, readiness, err := prepareUnavailableTestResumeOrigin(
		ctx,
		operation,
		handoff.Service,
		handoff.Revision,
	)
	if err != nil {
		return result, err
	}

	settings, _, err := operation.client.workerSettings(
		ctx,
		operation.account,
		operation.name,
		false,
	)
	if err != nil {
		return result, err
	}

	retained, err := settings.handoff(operation.configuration)
	if err != nil {
		return result, err
	}

	if retained != handoff {
		return result, errSavedHandoffChanged
	}

	receipt := readiness

	receipt.Candidate, receipt.Revision, receipt.VersionBefore, receipt.Interrupted =
		handoff.Service, handoff.Revision, before, true
	if err := WriteJSON(
		filepath.Join(operation.directory, "handoff-resume-readiness.json"),
		receipt,
	); err != nil {
		return result, err
	}

	options.Directory += "-resumed"
	options.Configuration = configurationPath

	result, err = Deploy(ctx, options)
	if err != nil {
		return result, err
	}

	_, after, err := operation.testWorkerVersionNamed(ctx, handoff.Service)
	if err != nil {
		return result, err
	}

	if before != after || !result.Promoted ||
		result.PendingHandoffRevision != handoff.Revision {
		return result, errResumedHandoffChanged
	}

	receipt.VersionAfter, receipt.ResumedPromoted = after, result.Promoted

	return result, WriteJSON(filepath.Join(operation.directory, "handoff-resume.json"), receipt)
}

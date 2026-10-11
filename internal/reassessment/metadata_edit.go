package reassessment

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
)

// ScheduleMetadataEdit updates a durable due time only for a distinct delivery.
func (coordinator *Coordinator) ScheduleMetadataEdit(ctx context.Context, job domain.ReviewJob) (*Record, bool, error) {
	if !coordinator.Settings.Enabled || !job.MetadataOnly || coordinator.Settings.MetadataSettleInterval == "" {
		return nil, false, nil
	}
	interval, err := time.ParseDuration(coordinator.Settings.MetadataSettleInterval)
	if err != nil {
		coordinator.Logger.ErrorContext(ctx, "parse metadata settle interval", "err", err)
		return nil, false, fmt.Errorf("parse metadata settle interval: %w", err)
	}
	if interval == 0 {
		return nil, false, nil
	}
	current, err := coordinator.GitHub.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		coordinator.Logger.ErrorContext(ctx, "read edited pull request", "err", err)
		return nil, false, fmt.Errorf("read edited pull request: %w", err)
	}
	if !current.EligibilityKnown || !current.MetadataKnown {
		return nil, false, errors.New("edited pull request metadata is incomplete")
	}
	if current.Head != job.Head {
		return nil, false, nil
	}
	key := coordinator.key(job)
	response, err := coordinator.Store.Send(ctx, Mutation{Action: "query", Key: key, ExpectedVersion: 0, Record: nil})
	if err != nil {
		return nil, false, err
	}
	prior := response.Record
	if prior != nil && (prior.OriginDeliveryID == job.DeliveryID || slices.Contains(prior.SourceDeliveries, job.DeliveryID)) {
		return prior, true, nil
	}
	now := coordinator.Clock()
	record := coordinator.newGeneration(key, job, now)
	record.Job = job
	record.Job.ReassessmentID = record.ID
	record.Job.ReassessmentGeneration = record.Generation
	record.Head = job.Head
	record.MetadataRevision = diff.PullRequestRevision(current)
	record.MetadataFingerprint = diff.MetadataRevision(diff.PullRequestMetadata(current, nil))
	record.Phase = "waiting"
	record.Reason = "metadata_edit_settling"
	record.NotBeforeMS = now.Add(interval).UnixMilli()
	version := int64(0)
	if prior != nil {
		version = prior.Version
		if prior.Job.MetadataOnly && prior.Head == job.Head && prior.Phase != "terminal" {
			record.SourceDeliveries = append([]string(nil), prior.SourceDeliveries...)
			if !slices.Contains(record.SourceDeliveries, prior.Job.DeliveryID) {
				record.SourceDeliveries = append(record.SourceDeliveries, prior.Job.DeliveryID)
			}
		}
	}
	record.SourceDeliveries = append(record.SourceDeliveries, job.DeliveryID)
	response, err = coordinator.Store.Send(ctx, Mutation{Action: "transition", Key: key, ExpectedVersion: version, Record: &record})
	if err != nil {
		return nil, false, err
	}
	if !response.Applied {
		return nil, false, errors.New("metadata queue changed before persistence")
	}
	coordinator.log(ctx, "metadata edit queued", *response.Record)
	return response.Record, true, nil
}

// SupersedeMetadataEdit rejects late metadata outcomes with a new generation.
func (coordinator *Coordinator) SupersedeMetadataEdit(ctx context.Context, job domain.ReviewJob) (*Record, *Record, error) {
	if !coordinator.Settings.Enabled || job.MetadataOnly {
		return nil, nil, nil
	}
	key := coordinator.key(job)
	response, err := coordinator.Store.Send(ctx, Mutation{Action: "query", Key: key, ExpectedVersion: 0, Record: nil})
	if err != nil {
		return nil, nil, err
	}
	prior := response.Record
	if prior == nil || !prior.Job.MetadataOnly || prior.Phase == "terminal" {
		return nil, nil, nil
	}
	current, err := coordinator.GitHub.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		coordinator.Logger.ErrorContext(ctx, "read superseding pull request", "err", err)
		return nil, nil, fmt.Errorf("read superseding pull request: %w", err)
	}
	if !current.EligibilityKnown || !current.MetadataKnown {
		return nil, nil, errors.New("superseding pull request metadata is incomplete")
	}
	if current.Head != job.Head {
		return nil, nil, nil
	}
	now := coordinator.Clock()
	record := coordinator.newGeneration(key, job, now)
	record.Job = job
	record.Job.ReassessmentID = record.ID
	record.Job.ReassessmentGeneration = record.Generation
	record.Head = job.Head
	record.MetadataRevision = diff.PullRequestRevision(current)
	record.Phase = "running"
	record.Reason = "metadata_edit_superseded"
	record.SupersededMetadataHead = prior.Head
	record.AttemptID = job.DeliveryID
	record.Attempts = 1
	record.NotBeforeMS = now.UnixMilli()
	record.SourceDeliveries = append([]string(nil), prior.SourceDeliveries...)
	if !slices.Contains(record.SourceDeliveries, prior.Job.DeliveryID) {
		record.SourceDeliveries = append(record.SourceDeliveries, prior.Job.DeliveryID)
	}
	record.SourceDeliveries = append(record.SourceDeliveries, job.DeliveryID)
	response, err = coordinator.Store.Send(ctx, Mutation{Action: "transition", Key: key, ExpectedVersion: prior.Version, Record: &record})
	if err != nil {
		return nil, nil, err
	}
	if !response.Applied {
		return nil, nil, errors.New("metadata queue changed before full review persistence")
	}
	coordinator.log(ctx, "metadata edit superseded", *response.Record)
	return response.Record, prior, nil
}

// WakeUnqueuedReview preserves durable execution after admission cannot enqueue work.
func (coordinator *Coordinator) WakeUnqueuedReview(ctx context.Context, job domain.ReviewJob) error {
	if !coordinator.Settings.Enabled {
		return nil
	}
	key := coordinator.key(job)
	response, err := coordinator.Store.Send(ctx, Mutation{Action: "query", Key: key, ExpectedVersion: 0, Record: nil})
	if err != nil {
		return err
	}
	record := response.Record
	if record == nil || record.Phase != "running" || record.Job.DeliveryID != job.DeliveryID {
		return nil
	}
	record.NotBeforeMS = coordinator.Clock().UnixMilli()
	record.Dispatch = false
	response, err = coordinator.Store.Send(ctx, Mutation{Action: "transition", Key: key, ExpectedVersion: record.Version, Record: record})
	if err != nil {
		return err
	}
	if !response.Applied {
		return errors.New("unqueued review changed before wake persistence")
	}
	return nil
}

func (coordinator *Coordinator) planMetadataEdit(ctx context.Context, record Record, live LiveSnapshot, now time.Time) Record {
	if live.FailureStage != "" {
		return coordinator.deferAfterFailure(ctx, record, now, live.FailureStage, errors.New("metadata review prerequisites are unavailable"))
	}
	current := live.PullRequest
	if current.State != "open" || current.Merged || current.Draft {
		return coordinator.terminal(record, "ineligible", now)
	}
	if current.Head != record.Head {
		return coordinator.terminal(record, "head_moved", now)
	}
	fingerprint := diff.MetadataRevision(diff.PullRequestMetadata(current, nil))
	changed := record.MetadataFingerprint != "" && fingerprint != record.MetadataFingerprint
	record.MetadataRevision = diff.PullRequestRevision(current)
	record.MetadataFingerprint = fingerprint
	if changed {
		interval, _ := time.ParseDuration(coordinator.Settings.MetadataSettleInterval)
		record.Phase = "waiting"
		record.Reason = "metadata_edit_settling"
		record.NotBeforeMS = now.Add(interval).UnixMilli()
		return record
	}
	if record.Phase == "waiting" && record.NotBeforeMS > now.UnixMilli() {
		return record
	}
	var snapshot Snapshot
	snapshot.PullRequest = current
	snapshot.LatestCheck = live.Check
	snapshot.LatestFound = live.CheckFound
	check, found, err := coordinator.GitHub.FindCheckRunByExternalID(ctx, record.Job.InstallationID, record.Job.Repository, record.Head, config.ReviewCheckName, record.Job.DeliveryID)
	if err != nil {
		return coordinator.deferAfterFailure(ctx, record, now, "attempt_check", err)
	}
	snapshot.AttemptCheck = check
	snapshot.AttemptFound = found
	return coordinator.Decide(record, snapshot, now)
}

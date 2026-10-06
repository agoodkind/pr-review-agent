package reassessment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// AcceptsOutcome rejects results from a previous queue lifecycle.
func (record Record) AcceptsOutcome(job domain.ReviewJob) bool {
	if job.ReassessmentGeneration != "" && job.ReassessmentGeneration != record.Generation {
		return false
	}
	if job.AutomaticReassessment {
		return job.ReassessmentGeneration == record.Generation && job.DeliveryID == record.AttemptID
	}
	if record.Job.ReassessmentGeneration != "" && job.ReassessmentGeneration == "" {
		return job.DeliveryID == record.OriginDeliveryID || slices.Contains(record.SourceDeliveries, job.DeliveryID)
	}
	return true
}

// Reactivate starts a fresh evaluation without resetting paid chunk checkpoints.
func (coordinator *Coordinator) Reactivate(prior *Record, job domain.ReviewJob, pullRequest githubapp.PullRequest, now time.Time) (Record, bool) {
	var empty Record
	if !coordinator.Settings.Enabled || !pullRequest.EligibilityKnown || !pullRequest.MetadataKnown || pullRequest.State != "open" || pullRequest.Merged || pullRequest.Draft {
		return empty, false
	}
	if prior != nil && (prior.OriginDeliveryID == job.DeliveryID || slices.Contains(prior.SourceDeliveries, job.DeliveryID)) {
		return *prior, false
	}
	job.Head = pullRequest.Head
	job.Forced = false
	job.RefreshVerdict = false
	if prior != nil && prior.Head == pullRequest.Head {
		job.ThreadRootCommentID = prior.Job.ThreadRootCommentID
	}
	record := coordinator.newGeneration(coordinator.key(job), job, now)
	record.Head = pullRequest.Head
	record.MetadataRevision = diff.PullRequestRevision(pullRequest)
	record.Job = job
	record.Job.ReassessmentID = record.ID
	record.Job.ReassessmentGeneration = record.Generation
	record.SourceDeliveries = []string{job.DeliveryID}
	record.Phase = "waiting"
	record.Reason = "reactivated"
	record.NotBeforeMS = now.UnixMilli()
	return record, true
}

// Reevaluate persists lifecycle evaluation before acknowledging its delivery.
func (coordinator *Coordinator) Reevaluate(ctx context.Context, job domain.ReviewJob) (bool, error) {
	if !coordinator.Settings.Enabled {
		return false, nil
	}
	pullRequest, err := coordinator.GitHub.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		coordinator.Logger.WarnContext(ctx, "read reactivated pull request", slog.String("err", err.Error()))
		return false, fmt.Errorf("read reactivated pull request: %w", err)
	}
	if !pullRequest.EligibilityKnown || !pullRequest.MetadataKnown {
		return false, errors.New("reactivated pull request eligibility is unknown")
	}
	if pullRequest.State != "open" || pullRequest.Merged || pullRequest.Draft {
		return true, nil
	}
	key := coordinator.key(job)
	response, err := coordinator.Store.Send(ctx, Mutation{Action: "query", Key: key, ExpectedVersion: 0, Record: nil})
	if err != nil {
		return false, err
	}
	renewed, changed := coordinator.Reactivate(response.Record, job, pullRequest, coordinator.Clock())
	if !changed {
		return true, nil
	}
	version := int64(0)
	if response.Record != nil {
		version = response.Record.Version
	}
	response, err = coordinator.Store.Send(ctx, Mutation{Action: "transition", Key: key, ExpectedVersion: version, Record: &renewed})
	if err != nil {
		return false, err
	}
	if !response.Applied {
		return false, errors.New("reactivated queue changed before persistence")
	}
	coordinator.log(ctx, "reassessment queued", *response.Record)
	return true, nil
}

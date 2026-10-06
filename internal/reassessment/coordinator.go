package reassessment

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"goodkind.io/pr-review-agent/internal/clock"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// Record retains one generation and its stable attempt identity across restarts.
type Record struct {
	RebuildEvidence  bool                     `json:"rebuild_evidence"`
	Dispatch         bool                     `json:"dispatch"`
	SourceDeliveries []string                 `json:"source_deliveries"`
	ID               string                   `json:"id"`
	Key              string                   `json:"key"`
	Version          int64                    `json:"version"`
	Generation       string                   `json:"generation"`
	OriginDeliveryID string                   `json:"origin_delivery_id"`
	Job              domain.ReviewJob         `json:"job"`
	Head             domain.HeadSHA           `json:"head"`
	MetadataRevision string                   `json:"metadata_revision"`
	Outcome          domain.AssessmentOutcome `json:"outcome"`
	Phase            string                   `json:"phase"`
	Reason           string                   `json:"reason"`
	CreatedAtMS      int64                    `json:"created_at_ms"`
	ExpiresAtMS      int64                    `json:"expires_at_ms"`
	RetainUntilMS    int64                    `json:"retain_until_ms"`
	NotBeforeMS      int64                    `json:"not_before_ms"`
	Attempts         int                      `json:"attempts"`
	AttemptID        string                   `json:"attempt_id"`
}

// GitHub rereads eligibility and check outcomes before retry admission.
type GitHub interface {
	ListIssueComments(context.Context, int64, domain.Repository, int) ([]githubapp.IssueComment, error)
	GetPullRequest(context.Context, int64, domain.Repository, int) (githubapp.PullRequest, error)
	FindCheckRun(context.Context, int64, domain.Repository, domain.HeadSHA, string) (githubapp.CheckRun, bool, error)
	FindCheckRunByExternalID(context.Context, int64, domain.Repository, domain.HeadSHA, string, string) (githubapp.CheckRun, bool, error)
}

// Availability checks app counters without sending model requests.
type Availability interface {
	NextAvailable(context.Context) (time.Time, error)
}

// Coordinator resumes checkpointed review work without forcing another full review.
type Coordinator struct {
	BotLogin  string
	Discovery InventoryGitHub
	Clock     clock.Clock
	Settings  config.Reassessment
	Store     *Client
	GitHub    GitHub
	Providers Availability
	Logger    *slog.Logger
}

func (coordinator *Coordinator) key(job domain.ReviewJob) string {
	return fmt.Sprintf("%d:%s", job.InstallationID, job.Key())
}

func (coordinator *Coordinator) delayed(now time.Time, attempts int) time.Time {
	delay, _ := time.ParseDuration(coordinator.Settings.InitialDelay)
	maximum, _ := time.ParseDuration(coordinator.Settings.MaximumDelay)
	for index := 0; index < attempts && delay < maximum; index++ {
		delay = min(delay*2, maximum)
	}
	var entropy [1]byte
	_, _ = rand.Read(entropy[:])
	delay += time.Duration(entropy[0]) * (delay / 10) / 255
	return now.Add(delay)
}

func (coordinator *Coordinator) log(ctx context.Context, message string, record Record) {
	coordinator.Logger.InfoContext(ctx, message,
		slog.String("reassessment_id", record.ID), slog.String("generation", record.Generation),
		slog.String("origin_delivery_id", record.OriginDeliveryID), slog.String("attempt_id", record.AttemptID),
		slog.String("repository", record.Job.Repository.Owner+"/"+record.Job.Repository.Name), slog.Int("pull_request", record.Job.Number),
		slog.String("head", string(record.Head)), slog.String("phase", record.Phase), slog.String("reason", record.Reason),
		slog.Int("attempts", record.Attempts), slog.Int64("not_before_ms", record.NotBeforeMS), slog.Int64("check_run_id", record.Outcome.CheckRunID),
		slog.Any("failure_classes", record.Outcome.FailureClasses))
}

func (coordinator *Coordinator) terminal(record Record, reason string, now time.Time) Record {
	record.Phase = "terminal"
	record.Reason = reason
	duration, _ := time.ParseDuration(coordinator.Settings.TerminalRetention)
	record.RetainUntilMS = now.Add(duration).UnixMilli()
	return record
}

// Observe requires durable retry intent before the check becomes terminal.
func (coordinator *Coordinator) Observe(ctx context.Context, job domain.ReviewJob, outcome domain.AssessmentOutcome) (domain.AssessmentOutcome, error) {
	if !coordinator.Settings.Enabled || outcome.Disposition == domain.AssessmentInterrupted {
		return outcome, nil
	}
	key := coordinator.key(job)
	response, err := coordinator.Store.Send(ctx, Mutation{Action: "query", Key: key, ExpectedVersion: 0, Record: nil})
	if err != nil {
		return outcome, err
	}
	prior := response.Record
	now := coordinator.Clock()
	retry := outcome.Disposition == domain.AssessmentFailed || outcome.Disposition == domain.AssessmentIncomplete || outcome.Disposition == domain.AssessmentDeclined && coordinator.Settings.RetryDeclined
	if job.AutomaticReassessment && prior == nil || prior != nil && !prior.AcceptsOutcome(job) {
		return outcome, nil
	}
	if err := coordinator.recordPrivateOutcome(ctx, job, outcome, prior); err != nil {
		return outcome, err
	}
	if !retry && prior == nil {
		return outcome, nil
	}
	var record Record
	version := int64(0)
	if prior != nil {
		record = *prior
		version = prior.Version
	}
	if prior == nil || prior.Head != job.Head || prior.Phase == "terminal" {
		record = coordinator.newGeneration(key, job, now)
	}
	if !retry && prior != nil && prior.Head != job.Head {
		return outcome, nil
	}
	record.Job = job
	if !job.AutomaticReassessment && !slices.Contains(record.SourceDeliveries, job.DeliveryID) {
		record.SourceDeliveries = append(record.SourceDeliveries, job.DeliveryID)
	}
	record.Head = job.Head
	record.MetadataRevision = outcome.MetadataRevision
	record.Outcome = outcome
	record.Phase = "waiting"
	record.Reason = string(outcome.Disposition)
	record.NotBeforeMS = coordinator.delayed(now, record.Attempts).UnixMilli()
	record.NotBeforeMS = max(record.NotBeforeMS, outcome.AvailableAtMS)
	if !retry {
		record.Phase = "confirming"
	}
	response, err = coordinator.Store.Send(ctx, Mutation{Action: "transition", Key: key, ExpectedVersion: version, Record: &record})
	if err != nil {
		return outcome, err
	}
	if !response.Applied {
		return outcome, errors.New("reassessment record changed before outcome persistence")
	}
	if retry {
		outcome.ReassessmentID = record.ID
		outcome.RetryAtMS = record.NotBeforeMS
		coordinator.log(ctx, "reassessment queued", *response.Record)
	} else {
		coordinator.log(ctx, "reassessment outcome recorded", *response.Record)
	}
	return outcome, nil
}

// DeliveryScheduled prevents replay from bypassing a persisted retry delay.
func (coordinator *Coordinator) DeliveryScheduled(ctx context.Context, job *domain.ReviewJob) (bool, error) {
	if !coordinator.Settings.Enabled || job.AutomaticReassessment {
		return false, nil
	}
	response, err := coordinator.Store.Send(ctx, Mutation{Action: "query", Key: coordinator.key(*job), ExpectedVersion: 0, Record: nil})
	if err != nil {
		return false, err
	}
	if response.Record != nil {
		if response.Record.Phase != "terminal" {
			job.ReassessmentGeneration = response.Record.Generation
		}
		if response.Record.OriginDeliveryID == job.DeliveryID {
			return true, nil
		}
		return slices.Contains(response.Record.SourceDeliveries, job.DeliveryID), nil
	}
	return false, nil
}

// Plan defers uncertain eligibility without consuming another retry attempt.
func (coordinator *Coordinator) Plan(ctx context.Context, record Record) Record {
	record.Dispatch = false
	now := coordinator.Clock()
	if !coordinator.Settings.Enabled {
		return coordinator.terminal(record, "disabled", now)
	}
	target, err := coordinator.Store.ReadTarget(ctx, record.Key)
	if err != nil {
		return coordinator.deferAfterFailure(ctx, record, now, "target", err)
	}
	if target == nil {
		if err := coordinator.Track(ctx, record.Job); err != nil {
			return coordinator.deferAfterFailure(ctx, record, now, "target", err)
		}
		target, err = coordinator.Store.ReadTarget(ctx, record.Key)
		if err != nil || target == nil {
			return coordinator.deferAfterFailure(ctx, record, now, "target", errors.New("target registration could not be verified"))
		}
	}
	live := coordinator.ReadLive(ctx, record.Job)
	repair := coordinator.Repair(*target, &record, live, now)
	applied, err := coordinator.Store.WriteTarget(ctx, repair.Target, repair.ExpectedTargetVersion, record.Version, record.Generation)
	if err != nil {
		return coordinator.deferAfterFailure(ctx, record, now, "target", err)
	}
	if !applied {
		return coordinator.deferAfterFailure(ctx, record, now, "target", errors.New("target observation changed before persistence"))
	}
	if repair.Record != nil {
		return *repair.Record
	}
	if repair.RepairKind != "consistent_pending" {
		record.Reason = repair.RepairKind
		record.NotBeforeMS = repair.Target.NotBeforeMS
		return record
	}
	pr := live.PullRequest
	var snapshot Snapshot
	snapshot.PullRequest = pr
	snapshot.LatestCheck = live.Check
	snapshot.LatestFound = live.CheckFound
	if record.RebuildEvidence {
		snapshot.LatestFound = false
	}
	if record.Phase == "running" || record.Phase == "confirming" {
		priorCheck, priorFound, priorErr := coordinator.GitHub.FindCheckRunByExternalID(ctx, record.Job.InstallationID, record.Job.Repository, record.Head, config.ReviewCheckName, record.Job.DeliveryID)
		if priorErr != nil {
			return coordinator.deferAfterFailure(ctx, record, now, "attempt_check", priorErr)
		}
		snapshot.AttemptCheck = priorCheck
		snapshot.AttemptFound = priorFound
	}
	available, err := coordinator.Providers.NextAvailable(ctx)
	if err != nil {
		return coordinator.deferAfterFailure(ctx, record, now, "provider_availability", err)
	}
	snapshot.AvailableAt = available
	return coordinator.Decide(record, snapshot, now)
}

// Snapshot separates current GitHub and quota evidence from retry decisions.
type Snapshot struct {
	PullRequest  githubapp.PullRequest
	LatestCheck  githubapp.CheckRun
	LatestFound  bool
	AttemptCheck githubapp.CheckRun
	AttemptFound bool
	AvailableAt  time.Time
}

// Decide uses current eligibility and metadata rather than a previous check conclusion.
func (coordinator *Coordinator) Decide(record Record, snapshot Snapshot, now time.Time) Record {
	record.Dispatch = false
	if !coordinator.Settings.Enabled {
		return coordinator.terminal(record, "disabled", now)
	}
	pr := snapshot.PullRequest
	if !pr.EligibilityKnown || !pr.MetadataKnown {
		record.Reason = "pull_request_unavailable"
		record.NotBeforeMS = coordinator.delayed(now, record.Attempts).UnixMilli()
		return record
	}
	if pr.State != "open" || pr.Merged || pr.Draft {
		reason := "closed"
		if pr.Merged {
			reason = "merged"
		} else if pr.Draft {
			reason = "draft"
		}
		return coordinator.terminal(record, reason, now)
	}
	revision := diff.PullRequestRevision(pr)
	if record.Head != pr.Head || record.MetadataRevision != "" && record.MetadataRevision != revision {
		record.Head = pr.Head
		record.MetadataRevision = revision
		record.Generation = rand.Text()
		record.AttemptID = ""
		record.Phase = "waiting"
		record.Attempts = 0
		record.CreatedAtMS = now.UnixMilli()
		ttl, _ := time.ParseDuration(coordinator.Settings.TTL)
		record.ExpiresAtMS = 0
		if ttl > 0 {
			record.ExpiresAtMS = now.Add(ttl).UnixMilli()
		}
	}
	if record.ExpiresAtMS > 0 && now.UnixMilli() >= record.ExpiresAtMS {
		return coordinator.terminal(record, "expired", now)
	}
	if record.Phase != "running" && record.Phase != "confirming" && coordinator.Settings.MaximumAttempts > 0 && record.Attempts >= coordinator.Settings.MaximumAttempts {
		return coordinator.terminal(record, "attempts_exhausted", now)
	}
	latest := snapshot.LatestCheck
	if snapshot.LatestFound && completedAt(latest, pr.Head, revision) {
		return coordinator.terminal(record, "completed", now)
	}
	if attemptSettled(record, snapshot) {
		if completedAt(snapshot.AttemptCheck, pr.Head, revision) {
			return coordinator.terminal(record, "completed", now)
		}
		if snapshot.AttemptCheck.Outcome.Disposition == domain.AssessmentDeclined && !coordinator.Settings.RetryDeclined {
			return coordinator.terminal(record, "declined", now)
		}
		record.Phase = "waiting"
	}
	if snapshot.AvailableAt.After(now) {
		record.NotBeforeMS = snapshot.AvailableAt.UnixMilli() + 1000
		record.Reason = "quota_wait"
		if record.Phase != "running" {
			record.Phase = "waiting"
		}
		return record
	}
	return coordinator.dispatchRecord(record, now)
}

func (coordinator *Coordinator) dispatchRecord(record Record, now time.Time) Record {
	if record.Phase != "running" && record.Phase != "confirming" {
		record.AttemptID = "reassessment-" + rand.Text()
		record.Attempts++
	}
	record.Job.Head = record.Head
	record.Job.DeliveryID = record.AttemptID
	record.Job.CheckRunID = 0
	record.Job.CheckRunStatus = ""
	record.Job.CheckRunConclusion = ""
	record.Job.Forced = false
	var settings domain.ReviewSettings
	record.Job.Settings = settings
	record.Job.AutomaticReassessment = true
	record.Job.ReassessmentID = record.ID
	record.Job.ReassessmentGeneration = record.Generation
	record.Phase = "running"
	record.Dispatch = true
	record.NotBeforeMS = coordinator.delayed(now, 0).UnixMilli()
	return record
}

func (coordinator *Coordinator) newGeneration(key string, job domain.ReviewJob, now time.Time) Record {
	var record Record
	record.ID = rand.Text()
	record.Key = key
	record.Generation = rand.Text()
	record.OriginDeliveryID = job.DeliveryID
	record.CreatedAtMS = now.UnixMilli()
	ttl, _ := time.ParseDuration(coordinator.Settings.TTL)
	if ttl > 0 {
		record.ExpiresAtMS = now.Add(ttl).UnixMilli()
	}
	return record
}

func (coordinator *Coordinator) deferAfterFailure(ctx context.Context, record Record, now time.Time, stage string, cause error) Record {
	coordinator.Logger.WarnContext(ctx, "reassessment prerequisite unavailable", slog.String("stage", stage), slog.String("err", cause.Error()))
	record.Reason = stage + "_unavailable"
	record.NotBeforeMS = coordinator.delayed(now, record.Attempts).UnixMilli()
	return record
}

func completedAt(check githubapp.CheckRun, head domain.HeadSHA, revision string) bool {
	return check.Status == "completed" && check.Outcome.Disposition == domain.AssessmentCompleted && check.Outcome.Head == head && check.Outcome.MetadataRevision == revision
}

func attemptSettled(record Record, snapshot Snapshot) bool {
	return (record.Phase == "running" || record.Phase == "confirming") && snapshot.AttemptFound && snapshot.AttemptCheck.Status == "completed" && snapshot.AttemptCheck.Conclusion != "cancelled"
}

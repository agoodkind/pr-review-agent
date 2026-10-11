package app

import (
	"context"
	"errors"
	"fmt"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reassessment"
)

type metadataCheckCompleter interface {
	CancelMetadataCheck(context.Context, domain.ReviewJob) error
}

func (handler *handler) scheduleMetadataEdit(ctx context.Context, job *domain.ReviewJob) (bool, error) {
	logger := gklog.L(ctx)
	if handler.reassessment == nil {
		return false, nil
	}
	if !job.MetadataOnly {
		return false, handler.supersedeMetadataEdit(ctx, job)
	}
	record, scheduled, err := handler.reassessment.ScheduleMetadataEdit(ctx, *job)
	if err != nil {
		logger.ErrorContext(ctx, "schedule metadata edit", "err", err)
		return false, fmt.Errorf("schedule metadata edit: %w", err)
	}
	if !scheduled {
		return false, nil
	}
	if err := handler.completeSupersededMetadataChecks(ctx, *record); err != nil {
		return false, err
	}
	return true, nil
}

func (handler *handler) supersedeMetadataEdit(ctx context.Context, job *domain.ReviewJob) error {
	logger := gklog.L(ctx)
	replacement, prior, err := handler.reassessment.SupersedeMetadataEdit(ctx, *job)
	if err != nil {
		logger.ErrorContext(ctx, "supersede metadata edit", "err", err)
		return fmt.Errorf("supersede metadata edit: %w", err)
	}
	if replacement == nil {
		return nil
	}
	job.ReassessmentID = replacement.ID
	job.ReassessmentGeneration = replacement.Generation
	return handler.completeMetadataChecks(ctx, *prior, false)
}

func (handler *handler) completeSupersededMetadataChecks(ctx context.Context, record reassessment.Record) error {
	return handler.completeMetadataChecks(ctx, record, true)
}

func (handler *handler) completeMetadataChecks(ctx context.Context, record reassessment.Record, keepCurrent bool) error {
	logger := gklog.L(ctx)
	for deliveryID, head := range record.MetadataCheckHeads {
		if keepCurrent && deliveryID == record.Job.DeliveryID {
			continue
		}
		check, found, err := handler.reassessment.GitHub.FindCheckRunByExternalID(ctx, record.Job.InstallationID, record.Job.Repository, head, config.ReviewCheckName, deliveryID)
		if err != nil {
			logger.ErrorContext(ctx, "read superseded metadata check", "err", err)
			return fmt.Errorf("read superseded metadata check: %w", err)
		}
		if !found || check.Status == "completed" {
			continue
		}
		job := record.Job
		job.Head = head
		job.DeliveryID = deliveryID
		job.CheckRunID = check.ID
		job.CheckRunStatus = check.Status
		job.CheckRunConclusion = check.Conclusion
		if err := handler.completeMetadataCheck(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

func (handler *handler) completeMetadataCheck(ctx context.Context, job domain.ReviewJob) error {
	logger := gklog.L(ctx)
	completer, available := handler.admitter.(metadataCheckCompleter)
	if !available {
		return errors.New("metadata check completion is unavailable")
	}
	if err := completer.CancelMetadataCheck(ctx, job); err != nil {
		logger.ErrorContext(ctx, "complete metadata check", "err", err)
		return fmt.Errorf("complete metadata check: %w", err)
	}
	return nil
}

func (handler *handler) completeTerminatedMetadataCheck(ctx context.Context, record reassessment.Record) error {
	if !record.Job.MetadataOnly || record.Phase != "terminal" || record.Reason == "completed" || record.Reason == "declined" {
		return nil
	}
	return handler.completeMetadataCheck(ctx, record.Job)
}

func (handler *handler) wakeUnqueuedReview(ctx context.Context, job domain.ReviewJob) {
	if handler.reassessment == nil {
		return
	}
	if err := handler.reassessment.WakeUnqueuedReview(ctx, job); err != nil {
		handler.logger.ErrorContext(ctx, "wake unqueued review", "err", err)
	}
}

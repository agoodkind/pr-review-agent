package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

const (
	checkFailureMixedUsage     = "Review stopped: the app exhausted its configured token limit, and a provider API reported no remaining usage."
	checkFailureRateLimited    = "Review stopped: a provider temporarily limited the request rate."
	checkFailureMixedRateLimit = "Review stopped: an app token limit was exhausted, and a provider temporarily limited the request rate."
)

func (service *Service) failCheck(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	progress Summary,
	stage string,
	cause error,
) error {
	if errors.Is(cause, context.Canceled) && ctx.Err() != nil {
		gklog.L(ctx).InfoContext(ctx, "review interrupted", slog.String("err", cause.Error()))
		return cause
	}
	return service.reportFailedCheck(
		ctx,
		job,
		checkRunID,
		progress,
		stage,
		service.presentedConclusion(failureClassesOf(cause), "failure"),
		cause,
	)
}

// reportFailedCheck publishes a failed attempt with the conclusion that
// describes whether a redelivery may resume it.
func (service *Service) reportFailedCheck(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	progress Summary,
	stage string,
	conclusion string,
	cause error,
) error {
	logger := gklog.L(ctx)
	progress.Failed = true
	var statuses []ProviderStatus
	collectProviderStatuses(cause, &statuses)
	recordAssessment(ctx, domain.AssessmentFailed, &progress, failureNames(cause), quotaRecovery(statuses))
	var dismissErr error
	if conclusion != checkConclusionCancelled {
		dismissErr = service.dismissFailureVerdict(ctx, job, cause)
	}
	title := failureTitle(stage, cause)
	detail := service.terminalFailureDetail(ctx, job, cause)
	checkSummary := detail + "\n\n" + RenderDetails(progress)
	var completeErr error
	if checkRunID != 0 {
		completeErr = service.completeCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			checkRunID,
			conclusion,
			title,
			checkSummary,
		)
		if completeErr != nil {
			logger.ErrorContext(ctx, "complete failed check run", slog.String("err", completeErr.Error()))
		}
	}
	service.writeFailureSummary(ctx, job, progress, title, detail)
	if completeErr != nil {
		return errors.Join(cause, dismissErr, fmt.Errorf("complete check run: %w", completeErr))
	}
	logger.ErrorContext(ctx, "review job failed", slog.String("err", cause.Error()))
	return &reportedFailureError{cause: errors.Join(cause, dismissErr)}
}

// failureTitle names why a review stopped, in the one line a reader sees in the
// checks list before opening anything.
//
// A stage name alone tells the reader where the run was rather than what went
// wrong, so the classes this service can recognize are named first and the
// stage is the fallback. The provider's own sentence is not one of the options:
// it can carry the request it failed on, an internal endpoint, or a credential,
// and a check run is as public and as permanent as a comment.
//
// A failed model call never reaches here. It leaves its chunk pending rather
// than failing the run, so the neutral check reports it instead.
func failureTitle(stage string, cause error) string {
	switch failureClassOf(cause) {
	case config.FailureDailyBudget:
		return checkFailureDailyBudget
	case config.FailureUsageExceeded:
		return checkFailureUsage
	case config.FailureRateLimited:
		return checkFailureRateLimited
	case config.FailureDeadline:
		return checkFailureDeadline
	case config.FailureUnavailable:
		return checkFailureUnavailable
	case config.FailurePanic:
		return checkFailurePanic
	case config.FailureOther:
	}
	if stage == "" {
		return checkSummaryFailure
	}
	return stage
}

// chunkFailureReason names why chunks went unread, in wording this service
// wrote, or an empty string when nothing classifies.
//
// It classifies rather than quotes for the same reason failureTitle does.
// Exhausted usage is the largest single cause in production, and a reader who
// sees only a chunk count cannot tell it apart from a provider outage.
func chunkFailureReason(failures []chunkFailure) string {
	classes := chunkFailureClasses(failures)
	if slices.Contains(classes, config.FailureDailyBudget) && slices.Contains(classes, config.FailureRateLimited) {
		return checkFailureMixedRateLimit
	}
	if slices.Contains(classes, config.FailureDailyBudget) && slices.Contains(classes, config.FailureUsageExceeded) {
		return checkFailureMixedUsage
	}
	switch chunkFailureClass(failures) {
	case config.FailureDailyBudget:
		return checkFailureDailyBudget
	case config.FailureUsageExceeded:
		return checkFailureUsage
	case config.FailureRateLimited:
		return checkFailureRateLimited
	case config.FailureDeadline:
		return checkFailureDeadline
	case config.FailureUnavailable:
		return checkFailureUnavailable
	case config.FailurePanic, config.FailureOther:
		return ""
	}
	return ""
}

func chunkFailureClass(failures []chunkFailure) config.FailureClass {
	classes := chunkFailureClasses(failures)
	for _, class := range classes {
		switch class {
		case config.FailureDailyBudget, config.FailureUsageExceeded, config.FailureRateLimited, config.FailureDeadline, config.FailureUnavailable:
			return class
		case config.FailurePanic, config.FailureOther:
		}
	}
	return config.FailureOther
}

func chunkFailureClasses(failures []chunkFailure) []config.FailureClass {
	classes := make([]config.FailureClass, 0, len(failures))
	for _, failure := range failures {
		classes = append(classes, failureClassesOf(failure.err)...)
	}
	return uniqueFailureClasses(classes)
}

// failureClassOf checks the daily budget first because its error also reports exhausted usage.
func failureClassOf(cause error) config.FailureClass {
	switch {
	case dailyBudgetExhausted(cause):
		return config.FailureDailyBudget
	case providerRateLimited(cause):
		return config.FailureRateLimited
	case usageExceeded(cause):
		return config.FailureUsageExceeded
	case errors.Is(cause, context.DeadlineExceeded):
		return config.FailureDeadline
	case providerUnavailable(cause):
		return config.FailureUnavailable
	case isChunkPanic(cause):
		return config.FailurePanic
	default:
		return config.FailureOther
	}
}

// presentedConclusion prevents an allowed failure from hiding a blocking failure in the same run.
func (service *Service) presentedConclusion(classes []config.FailureClass, blocking string) string {
	if len(classes) == 0 {
		return blocking
	}
	for _, class := range classes {
		if !service.failureAppearances.Passes(class) {
			return blocking
		}
	}
	return "success"
}

// failureClassesOf retains every joined cause so the check accounts for every failure.
func failureClassesOf(cause error) []config.FailureClass {
	if cause == nil {
		return nil
	}
	for current := cause; current != nil; {
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			classes := make([]config.FailureClass, 0, len(joined.Unwrap()))
			for _, err := range joined.Unwrap() {
				classes = append(classes, failureClassesOf(err)...)
			}
			return uniqueFailureClasses(classes)
		}
		wrapped, ok := current.(interface{ Unwrap() error })
		if !ok {
			break
		}
		current = wrapped.Unwrap()
	}
	return []config.FailureClass{failureClassOf(cause)}
}

func uniqueFailureClasses(classes []config.FailureClass) []config.FailureClass {
	if len(classes) == 0 {
		return []config.FailureClass{config.FailureOther}
	}
	seen := make(map[config.FailureClass]struct{}, len(classes))
	unique := make([]config.FailureClass, 0, len(classes))
	for _, class := range classes {
		if _, found := seen[class]; found {
			continue
		}
		seen[class] = struct{}{}
		unique = append(unique, class)
	}
	return unique
}

// publicFailureDetail points a reader at the cause instead of reprinting it.
//
// A model provider error can carry the request it failed on, an internal
// endpoint, or a credential, and none of that can be unpublished once it is in
// a pull request comment. The run identifier is enough to pull the whole cause
// out of the service log, which stays private.
func publicFailureDetail(job domain.ReviewJob) string {
	if job.DeliveryID == "" {
		return "The cause is recorded in this service's log for this run."
	}
	return "The cause is recorded in this service's log, under run identifier `" + job.DeliveryID + "`."
}

// usageExceededError is any provider error that reports exhausted usage.
type usageExceededError interface {
	UsageExceeded() bool
}

type dailyBudgetError interface {
	DailyBudgetExhausted() bool
}

func dailyBudgetExhausted(cause error) bool {
	var target dailyBudgetError
	return errors.As(cause, &target) && target.DailyBudgetExhausted()
}

type providerUnavailableError interface {
	ProviderUnavailable() bool
}

type rateLimitedError interface {
	RateLimited() bool
}

func providerRateLimited(cause error) bool {
	var target rateLimitedError
	return errors.As(cause, &target) && target.RateLimited()
}

func providerUnavailable(cause error) bool {
	var unavailable providerUnavailableError
	return errors.As(cause, &unavailable) && unavailable.ProviderUnavailable()
}

func usageExceeded(cause error) bool {
	var target usageExceededError
	if !errors.As(cause, &target) {
		return false
	}
	return target.UsageExceeded()
}

// writeFailureSummary states why the review stopped, in the service's one top
// level comment.
//
// It carries the checkpoint forward exactly as the comment already holds it,
// both the last reviewed commit and the pending chunk list. Whatever chunks
// this run finished are already recorded there, and whatever it did not remain
// owed, so the next run neither repeats work already done nor skips work never
// done. A failure here is logged and never masks the cause the caller reports.
func (service *Service) writeFailureSummary(
	ctx context.Context,
	job domain.ReviewJob,
	progress Summary,
	title string,
	detail string,
) {
	ctx, cancel := detachFromReviewDeadline(ctx, service.checkCompletionTimeout)
	defer cancel()

	logger := gklog.L(ctx)
	prose := RenderFailureBody(progress, title, detail)
	prose += reassessmentNotice(ctx)
	err := service.upsertSummaryCommentFrom(ctx, job, func(existing marker.State, _ string) summaryCommentContent {
		return summaryCommentContent{
			Prose: prose,
			State: marker.State{
				LastReviewed: existing.LastReviewed,
				RunID:        job.DeliveryID,
				Status:       marker.StateFailed,
				Pending:      existing.Pending,
				// The chunks already read carry forward for the same reason the
				// pending ones do. Dropping them here would send the next run
				// over work this pull request has already paid for.
				Completed: existing.Completed,
				// So does the forcing delivery. A failed run is not that delivery
				// finishing, and losing the record would make its own resume clear
				// the very checkpoint this notice is preserving.
				ForcedBy: existing.ForcedBy,
				// So does the recorded shortfall, which a failure cannot clear:
				// dropping it would let the next run advance the baseline over a
				// hunk that is still unread.
				Unread: existing.Unread,
			},
		}
	})
	if err != nil {
		logger.ErrorContext(ctx, "write failure summary", slog.String("err", err.Error()))
		return
	}
	logger.InfoContext(ctx, "failure summary written", slog.Bool("visible", true))
}

package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

type failureReviewState string

type reportedFailureError struct{ cause error }

func (failure *reportedFailureError) Error() string { return failure.cause.Error() }
func (failure *reportedFailureError) Unwrap() error { return failure.cause }

type (
	joinedFailure          interface{ Unwrap() []error }
	wrappedFailure         interface{ Unwrap() error }
	providerFailureStatus  interface{ ProviderStatus() ProviderStatus }
	providerFailureDetails interface{ ProviderFailureDetails() (int, string) }
)

const checkFailurePanic = "Review failed after an internal panic."

func (service *Service) finalizeFailure(ctx context.Context, job domain.ReviewJob, checkRunID int64, progress *reviewProgress, runErr *error) {
	var reported *reportedFailureError
	if *runErr == nil || errors.As(*runErr, &reported) || failureInterrupted(ctx, *runErr) {
		return
	}
	if recorder, ok := ctx.Value(outcomeKey{}).(*outcomeRecorder); ok && recorder.outcome.Disposition == domain.AssessmentInterrupted {
		return
	}
	*runErr = service.reportFailedCheck(ctx, job, checkRunID, progress.summary(service.now()), checkFailurePublish, service.presentedConclusion(failureClassesOf(*runErr), "failure"), *runErr)
}

// SetFailureVerdictPolicy treats an empty policy as dismiss_latest_block.
func (service *Service) SetFailureVerdictPolicy(policy config.FailureVerdictPolicy) {
	if policy == "" {
		policy = config.DismissLatestFailureVerdict
	}
	service.failureVerdicts = policy
}

func unexpectedFailure(cause error) bool {
	return slices.ContainsFunc(failureClassesOf(cause), unexpectedFailureClass)
}

func unexpectedFailureClass(class config.FailureClass) bool {
	switch class {
	case config.FailureUnavailable, config.FailureDeadline, config.FailurePanic, config.FailureOther:
		return true
	case config.FailureDailyBudget, config.FailureUsageExceeded, config.FailureRateLimited:
		return false
	}
	return false
}

func (service *Service) dismissFailureVerdict(ctx context.Context, job domain.ReviewJob, cause error) error {
	if cause == nil || failureInterrupted(ctx, cause) || !unexpectedFailure(cause) || service.failureVerdicts == config.PreserveFailureVerdict {
		return nil
	}
	ctx, cancel := service.publicationContext(ctx)
	defer cancel()
	logger := gklog.L(ctx)
	reviews, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.WarnContext(ctx, "read standing verdict after failure", slog.String("err", err.Error()))
		return fmt.Errorf("read standing verdict after failure: %w", err)
	}
	latest := latestOwnDecision(reviews, service.botLogin)
	if latest.State != reviewStateChangesRequested {
		return nil
	}
	var data reviewrules.PromptData
	message, err := service.reviewPolicy.Render("failure.dismiss", data)
	if err != nil {
		logger.WarnContext(ctx, "render failure dismissal", slog.String("err", err.Error()))
		return fmt.Errorf("render failure dismissal: %w", err)
	}
	message += "\n\n" + marker.FailureDismissal(latest.ID)
	if _, err := service.github.DismissReview(ctx, job.InstallationID, job.Repository, job.Number, latest.ID, message); err != nil {
		logger.WarnContext(ctx, "dismiss standing rejection after failure", slog.Int64("review_id", latest.ID), slog.String("err", err.Error()))
		return fmt.Errorf("dismiss standing rejection after failure: %w", err)
	}
	logger.InfoContext(ctx, "review failure verdict dismissed", slog.Int64("review_id", latest.ID), slog.String("failure_class", string(failureClassOf(cause))))
	return nil
}

func (service *Service) failureDismissalRecorded(ctx context.Context, job domain.ReviewJob, review githubapp.Review) (bool, error) {
	logger := gklog.L(ctx)
	dismissal, found, err := service.github.FindReviewDismissal(ctx, job.InstallationID, job.Repository, job.Number, review.ID)
	if err != nil {
		logger.WarnContext(ctx, "read failure dismissal provenance", slog.String("err", err.Error()))
		return false, fmt.Errorf("read failure dismissal provenance: %w", err)
	}
	if !found {
		return false, errors.New("review dismissal event is not available")
	}
	return dismissal.ReviewID == review.ID && dismissal.Actor == service.botLogin && marker.HasFailureDismissal(dismissal.Message, review.ID), nil
}

func latestOwnDecision(reviews []githubapp.Review, botLogin string) githubapp.Review {
	for _, review := range slices.Backward(reviews) {
		if review.Author != botLogin {
			continue
		}
		switch failureReviewState(review.State) {
		case reviewStateApproved, reviewStateChangesRequested, reviewStateDismissed:
			return review
		}
	}
	return emptyReview()
}

func joinedChunkFailures(ctx context.Context, failures []chunkFailure) error {
	causes := make([]error, 0, len(failures))
	for _, failure := range failures {
		if failure.err != nil {
			causes = append(causes, failure.err)
		}
	}
	cause := errors.Join(causes...)
	if cause != nil {
		logger := gklog.L(ctx)
		logger.WarnContext(ctx, "review chunks failed", slog.String("err", cause.Error()))
	}
	return cause
}

func failureInterrupted(ctx context.Context, cause error) bool {
	if errors.Is(cause, errHeadMoved) {
		return true
	}
	shutdown := shutdownFrom(ctx)
	return shutdown != nil && shutdown.Err() != nil
}

type terminalProviderFailure struct {
	status     ProviderStatus
	class      config.FailureClass
	httpStatus int
	code       string
}

func collectTerminalProviderFailures(cause error, failures *[]terminalProviderFailure) {
	if joined, ok := cause.(joinedFailure); ok {
		for _, child := range joined.Unwrap() {
			collectTerminalProviderFailures(child, failures)
		}
		return
	}
	if reported, ok := cause.(providerFailureStatus); ok {
		failure := terminalProviderFailure{status: reported.ProviderStatus(), class: failureClassOf(cause), httpStatus: 0, code: ""}
		var details providerFailureDetails
		if errors.As(cause, &details) {
			failure.httpStatus, failure.code = details.ProviderFailureDetails()
		}
		*failures = append(*failures, failure)
		return
	}
	if wrapped, ok := cause.(wrappedFailure); ok {
		collectTerminalProviderFailures(wrapped.Unwrap(), failures)
	}
}

func (service *Service) terminalFailureDetail(ctx context.Context, job domain.ReviewJob, cause error) string {
	detail := publicFailureDetail(job)
	if cause == nil || !unexpectedFailure(cause) {
		return detail
	}
	var failures []terminalProviderFailure
	collectTerminalProviderFailures(cause, &failures)
	var data reviewrules.PromptData
	for _, class := range failureClassesOf(cause) {
		if unexpectedFailureClass(class) {
			data.FailureClass = string(class)
			break
		}
	}
	template := "failure.service"
	for _, failure := range failures {
		if failure.status.Cause == ProviderRequestFailed && unexpectedFailureClass(failure.class) {
			data = service.terminalFailureData(failure)
			template = "failure.provider"
			break
		}
	}
	data.FailureClass = service.failureClassLabel(ctx, data.FailureClass)
	visible := service.renderFailureTemplate(ctx, template, data)
	var rows strings.Builder
	for _, failure := range failures {
		row := service.terminalFailureData(failure)
		row.FailureClass = service.failureClassLabel(ctx, row.FailureClass)
		rows.WriteString(service.renderFailureTemplate(ctx, "failure.row", row))
	}
	if rows.Len() > 0 {
		data.Input = rows.String()
		visible += "\n\n" + service.renderFailureTemplate(ctx, "failure.details", data)
	}
	return visible + "\n\n" + detail
}

func (service *Service) failureClassLabel(ctx context.Context, class string) string {
	var data reviewrules.PromptData
	return service.renderFailureTemplate(ctx, "failure.class."+class, data)
}

func (service *Service) terminalFailureData(failure terminalProviderFailure) reviewrules.PromptData {
	var data reviewrules.PromptData
	data.Provider = usageModelCell(failure.status.ProviderID)
	data.Model = usageModelCell(failure.status.Model)
	data.FailureClass = string(failure.class)
	data.FailureStatus = "not reported"
	if failure.httpStatus >= 100 && failure.httpStatus <= 599 {
		data.FailureStatus = fmt.Sprintf("HTTP %d", failure.httpStatus)
	}
	data.FailureCode = "not reported"
	if service.reviewPolicy.RecognizesFailureCode(failure.code) {
		data.FailureCode = failure.code
	}
	return data
}

func (service *Service) renderFailureTemplate(ctx context.Context, name string, data reviewrules.PromptData) string {
	text, err := service.reviewPolicy.Render(name, data)
	if err != nil {
		gklog.L(ctx).WarnContext(ctx, "render failure details", slog.String("template", name), slog.String("err", err.Error()))
	}
	return text
}

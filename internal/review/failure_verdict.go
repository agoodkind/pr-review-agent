package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

type reportedFailureError struct{ cause error }

func (failure *reportedFailureError) Error() string { return failure.cause.Error() }
func (failure *reportedFailureError) Unwrap() error { return failure.cause }

type (
	joinedFailure          interface{ Unwrap() []error }
	wrappedFailure         interface{ Unwrap() error }
	providerFailureStatus  interface{ ProviderStatus() ProviderStatus }
	providerFailureDetails interface{ ProviderFailureDetails() (int, string) }
)

const checkFailurePanic = "output.failure_panic"

const unreportedFailureField = "not reported"

var errFailureTemplateRender = errors.New("render failure details")

func (service *Service) finalizeFailure(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	progress *reviewProgress,
	runErr *error,
) {
	var reported *reportedFailureError
	if *runErr == nil || errors.As(*runErr, &reported) || failureInterrupted(ctx, *runErr) {
		return
	}

	if recorder, ok := ctx.Value(outcomeKey{}).(*outcomeRecorder); ok &&
		recorder.outcome.Disposition == domain.AssessmentInterrupted {
		return
	}

	*runErr = service.reportFailedCheck(
		ctx,
		job,
		checkRunID,
		progress.summary(service.now()),
		checkFailurePublish,
		service.presentedConclusion(failureClassesOf(*runErr), "failure"),
		*runErr,
	)
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
	case config.FailureUnavailable,
		config.FailureDeadline,
		config.FailurePanic,
		config.FailureOther:
		return true
	case config.FailureDailyBudget, config.FailureUsageExceeded, config.FailureRateLimited:
		return false
	}

	return false
}

func (service *Service) dismissFailureVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	cause error,
) error {
	if cause == nil || failureInterrupted(ctx, cause) || !unexpectedFailure(cause) ||
		service.failureVerdicts == config.PreserveFailureVerdict {
		return nil
	}

	ctx, cancel := service.publicationContext(ctx)
	defer cancel()

	logger := gklog.L(ctx)

	reviews, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.WarnContext(
			ctx,
			"read standing verdict after failure",
			slog.String("err", err.Error()),
		)

		return fmt.Errorf("read standing verdict after failure: %w", err)
	}

	latest := githubapp.LatestDecisionReview(reviews, service.botLogin)
	if latest.State != reviewStateChangesRequested {
		return nil
	}

	threads, err := service.github.ListReviewThreads(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
	)
	if err != nil {
		return fmt.Errorf("verify file findings before failure dismissal: %w", err)
	}

	if hasUnresolvedBotThread(threads, service.botLogin) {
		logger.InfoContext(
			ctx,
			"preserve unresolved file rejection after failure",
			"review_id",
			latest.ID,
		)

		return nil
	}

	comments, err := service.github.ListIssueComments(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
	)
	if err != nil {
		return fmt.Errorf("verify saved findings before failure dismissal: %w", err)
	}

	comment, found := findSummaryComment(comments, service.botLogin)

	saved, recorded, err := decodeMetadataRecord(comment.Body)
	if err != nil {
		return fmt.Errorf("read saved findings before failure dismissal: %w", err)
	}

	preserve, verificationErr := service.preserveMetadataRejection(
		ctx,
		job,
		latest,
		comments,
		recorded && len(saved.Findings) > 0,
		logger,
	)
	if verificationErr != nil {
		return verificationErr
	}

	if preserve {
		return nil
	}

	return service.publishFailureDismissal(ctx, job, latest, comment, found, cause)
}

func (service *Service) publishFailureDismissal(
	ctx context.Context,
	job domain.ReviewJob,
	latest githubapp.Review,
	comment githubapp.IssueComment,
	found bool,
	cause error,
) error {
	logger := gklog.L(ctx)
	var data reviewrules.PromptData

	lead, err := service.terminalFailureLead(ctx, cause)
	if err != nil {
		return err
	}

	data.Input = lead

	if found && comment.ID > 0 {
		data.SummaryURL = nativeSummaryURL(job.Repository, job.Number, comment.ID)
	} else {
		logger.WarnContext(ctx, "failure dismissal summary is unavailable")
	}

	message, err := service.reviewPolicy.Render("failure.dismiss", data)
	if err != nil {
		logger.WarnContext(ctx, "render failure dismissal", slog.String("err", err.Error()))
		return fmt.Errorf("render failure dismissal: %w", err)
	}

	message, err = renderOutputParts(message, marker.FailureDismissal(latest.ID))
	if err != nil {
		return err
	}

	dismissCtx, cancelDismiss := service.publicationContext(ctx)
	defer cancelDismiss()

	if _, err := service.github.DismissReview(
		dismissCtx,
		job.InstallationID,
		job.Repository,
		job.Number,
		latest.ID,
		message,
	); err != nil {
		logger.WarnContext(
			ctx,
			"dismiss standing rejection after failure",
			slog.Int64("review_id", latest.ID),
			slog.String("err", err.Error()),
		)

		return fmt.Errorf("dismiss standing rejection after failure: %w", err)
	}

	logger.InfoContext(
		ctx,
		"review failure verdict dismissed",
		slog.Int64("review_id", latest.ID),
		slog.String("failure_class", string(failureClassOf(cause))),
	)

	return nil
}

func (service *Service) preserveMetadataRejection(
	ctx context.Context,
	job domain.ReviewJob,
	latest githubapp.Review,
	comments []githubapp.IssueComment,
	savedBlocked bool,
	logger *slog.Logger,
) (bool, error) {
	if !validNativeMetadataBlock(latest) && !savedBlocked {
		return false, nil
	}

	blocked, verificationErr := service.savedMetadataStillBlocks(ctx, job, comments)
	if verificationErr != nil {
		return false, verificationErr
	}

	if blocked {
		logger.InfoContext(
			ctx,
			"preserve unresolved metadata rejection after failure",
			"review_id",
			latest.ID,
		)
	}

	return blocked, nil
}

func automaticFailureDismissal(
	dismissal githubapp.ReviewDismissal,
	botLogin string,
	reviewID int64,
) bool {
	return dismissal.ReviewID == reviewID && dismissal.Actor == botLogin &&
		marker.HasFailureDismissal(dismissal.Message, reviewID)
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
		failure := terminalProviderFailure{
			status:     reported.ProviderStatus(),
			class:      failureClassOf(cause),
			httpStatus: 0,
			code:       "",
		}

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

func (service *Service) terminalFailureDetail(
	ctx context.Context,
	job domain.ReviewJob,
	cause error,
) (string, error) {
	if _, validation := errors.AsType[*AssessmentValidationError](cause); validation {
		return "", nil
	}

	var rendering renderState
	detail := rendering.capture(publicFailureDetail(job))
	if cause == nil || !unexpectedFailure(cause) {
		return rendering.finish(detail)
	}

	var failures []terminalProviderFailure
	collectTerminalProviderFailures(cause, &failures)
	visible := rendering.capture(service.terminalFailureLead(ctx, cause))

	var rows strings.Builder

	for _, failure := range failures {
		row, err := service.terminalFailureData(failure)
		if err != nil {
			return "", err
		}

		row.FailureClass = rendering.capture(service.failureClassLabel(ctx, row.FailureClass))
		rows.WriteString(rendering.capture(service.renderFailureTemplate(ctx, "failure.row", row)))
	}

	if rows.Len() > 0 {
		var data reviewrules.PromptData

		data.Input = rows.String()
		visible = rendering.capture(
			renderOutputParts(
				visible,
				rendering.capture(service.renderFailureTemplate(ctx, "failure.details", data)),
			),
		)
	}

	return rendering.finish(rendering.capture(renderOutputParts(visible, detail)))
}

func terminalFailureClass(cause error) config.FailureClass {
	return failureClassOf(terminalFailureCause(cause))
}

func terminalFailureCause(cause error) error {
	for current := cause; current != nil; {
		if joined, ok := current.(joinedFailure); ok {
			for _, child := range slices.Backward(joined.Unwrap()) {
				if child != nil {
					return terminalFailureCause(child)
				}
			}
		}

		if _, ok := current.(providerFailureStatus); ok {
			return current
		}

		wrapped, ok := current.(wrappedFailure)
		if !ok {
			break
		}

		current = wrapped.Unwrap()
	}

	return cause
}

func (service *Service) terminalFailureLead(ctx context.Context, cause error) (string, error) {
	var rendering renderState
	terminal := terminalFailureCause(cause)

	var failures []terminalProviderFailure
	collectTerminalProviderFailures(terminal, &failures)

	var data reviewrules.PromptData

	data.FailureClass = string(failureClassOf(terminal))
	data.FailureStatus = unreportedFailureField

	var githubFailure githubapp.APIError
	if errors.As(terminal, &githubFailure) {
		data.FailureStatus = rendering.capture(
			renderOutputTemplate("output.http_status", reflect.ValueOf(githubFailure.StatusCode)),
		)
	}

	template := "failure.service"

	if len(failures) > 0 {
		var err error
		data, err = service.terminalFailureData(failures[len(failures)-1])
		if err != nil {
			return "", err
		}

		template = "failure.provider"
	}

	data.FailureClass = rendering.capture(service.failureClassLabel(ctx, data.FailureClass))

	return rendering.finish(rendering.capture(service.renderFailureTemplate(ctx, template, data)))
}

func (service *Service) failureClassLabel(ctx context.Context, class string) (string, error) {
	var data reviewrules.PromptData
	return service.renderFailureTemplate(ctx, "failure.class."+class, data)
}

func (service *Service) terminalFailureData(
	failure terminalProviderFailure,
) (reviewrules.PromptData, error) {
	var data reviewrules.PromptData
	var rendering renderState

	data.Provider = rendering.capture(usageModelCell(failure.status.ProviderID))
	data.Model = rendering.capture(usageModelCell(failure.status.Model))
	data.FailureClass = string(failure.class)

	data.FailureStatus = unreportedFailureField
	if failure.httpStatus >= 100 && failure.httpStatus <= 599 {
		data.FailureStatus = rendering.capture(
			renderOutputTemplate("output.http_status", reflect.ValueOf(failure.httpStatus)),
		)
	}

	data.FailureCode = unreportedFailureField
	if service.reviewPolicy.RecognizesFailureCode(failure.code) {
		data.FailureCode = failure.code
	}

	return data, rendering.err
}

func (service *Service) renderFailureTemplate(
	ctx context.Context,
	name string,
	data reviewrules.PromptData,
) (string, error) {
	text, err := service.reviewPolicy.Render(name, data)
	if err != nil {
		logger := gklog.L(ctx)
		logger.WarnContext(
			ctx,
			"render failure details",
			slog.String("template", name),
			slog.String("err", err.Error()),
		)

		return "", fmt.Errorf("%w: %w", errFailureTemplateRender, err)
	}

	return text, nil
}

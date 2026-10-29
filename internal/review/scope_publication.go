package review

import (
	"context"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

type pendingScopePublication struct {
	job        domain.ReviewJob
	checkRunID int64
	state      marker.State
	summary    Summary
	notice     string
	title      string
	conclusion string
	statuses   []ProviderStatus
}

func (service *Service) publishPendingScope(ctx context.Context, publication pendingScopePublication, progress *reviewProgress) error {
	service.enrichProviderPeriodUsage(ctx, &publication.summary)
	publicationCtx, cancel := service.publicationContext(ctx)
	defer cancel()
	if _, err := persistAssessment(publicationCtx, publication.checkRunID); err != nil {
		return err
	}
	body, err := renderPendingScopeBody(publication.summary, publication.notice, service.reviewPolicy, publication.statuses...)
	if err != nil {
		return err
	}
	if err := service.upsertSummaryComment(publicationCtx, publication.job, summaryCommentContent{
		Prose: body + reassessmentNotice(ctx), State: publication.state,
	}); err != nil {
		return service.failCheck(publicationCtx, publication.job, publication.checkRunID, progress.summary(service.now()), checkFailureSummary, err)
	}
	if err := service.completeCheckRun(publicationCtx, publication.job.InstallationID, publication.job.Repository, publication.checkRunID, publication.conclusion, publication.title, publication.notice+"\n\n"+RenderDetails(publication.summary, publication.statuses...)); err != nil {
		gklog.L(ctx).WarnContext(ctx, "Completing the review check failed", slog.String("err", err.Error()))
		return err
	}
	return nil
}

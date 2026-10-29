package review

import (
	"context"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// ProviderPeriodUsageLoader reads stored usage without changing admission decisions.
type ProviderPeriodUsageLoader interface {
	LoadProviderPeriodUsage(context.Context, []ProviderModelKey, time.Time) ([]ProviderPeriodUsage, error)
}

func (service *Service) enrichProviderPeriodUsage(ctx context.Context, summary *Summary) {
	enabled := false
	for _, column := range service.reviewPolicy.ProviderDetails() {
		if column == reviewrules.ProviderPeriodUsageColumn {
			enabled = true
		}
	}
	loader, available := service.model.(ProviderPeriodUsageLoader)
	if !enabled || !available {
		return
	}
	if len(summary.Usage.ProviderAttempts) == 0 || len(summary.Usage.PeriodUsage) > 0 {
		return
	}
	var keys []ProviderModelKey
	for _, attempt := range summary.Usage.ProviderAttempts {
		keys = append(keys, ProviderModelKey{ProviderID: attempt.Status.ProviderID, RequestedModel: attempt.Status.Model})
	}
	queryCtx, cancel := service.publicationContext(ctx)
	defer cancel()
	periods, err := loader.LoadProviderPeriodUsage(queryCtx, keys, service.now())
	if err != nil {
		gklog.L(ctx).WarnContext(ctx, "Read provider period usage failed", "err", err)
	}
	summary.Usage.PeriodUsage = periods
}

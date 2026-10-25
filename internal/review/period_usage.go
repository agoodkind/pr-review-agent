package review

import (
	"context"
	"slices"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// ProviderModelKey identifies a provider and requested-model report row.
type ProviderModelKey struct {
	ProviderID     string
	RequestedModel string
}

// ProviderPeriodUsage reports stored usage and configured price estimates for one period.
type ProviderPeriodUsage struct {
	ProviderID       string
	RequestedModel   string
	Window           quota.Window
	Bounds           quota.Bounds
	AsOfMS           int64
	HistoryStartMS   int64
	HistoryComplete  bool
	Available        bool
	TotalTokens      int64
	ReportedRequests int
	PricedRequests   int
	EstimatedCostUSD float64
	CostComplete     bool
}

// ProviderPeriodUsageLoader reads period totals after model usage delivery completes.
type ProviderPeriodUsageLoader interface {
	LoadProviderPeriodUsage(context.Context, []ProviderModelKey, time.Time) ([]ProviderPeriodUsage, error)
}

// RecordProviderUsageDeliveryFailure prevents unacknowledged current usage from appearing complete.
func RecordProviderUsageDeliveryFailure(ctx context.Context, key ProviderModelKey) {
	recorder, ok := ctx.Value(usageRecorderContextKey{}).(*UsageRecorder)
	if !ok || recorder == nil {
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.deliveryFailures[key] = true
}

// ProviderUsageDeliveryFailed reports a failed accounting delivery in the current run.
func ProviderUsageDeliveryFailed(ctx context.Context, providerID string) bool {
	recorder, ok := ctx.Value(usageRecorderContextKey{}).(*UsageRecorder)
	if !ok || recorder == nil {
		return false
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	for key := range recorder.deliveryFailures {
		if key.ProviderID == providerID {
			return true
		}
	}
	return false
}

func (service *Service) enrichProviderPeriodUsage(ctx context.Context, summary *Summary) {
	columns := service.reviewPolicy.ProviderDetails()
	if !slices.Contains(columns, reviewrules.ProviderPeriodUsageColumn) {
		return
	}
	loader, ok := service.model.(ProviderPeriodUsageLoader)
	if !ok {
		return
	}
	if recorder, found := ctx.Value(usageRecorderContextKey{}).(*UsageRecorder); found && recorder != nil {
		recorder.mu.Lock()
		if recorder.periodLoaded && recorder.periodVersion == recorder.totals.Requests {
			summary.Usage.PeriodUsage = slices.Clone(recorder.totals.PeriodUsage)
			summary.Usage.ProviderColumns = columns
			recorder.mu.Unlock()
			return
		}
		recorder.mu.Unlock()
	}
	loadContext, cancel := detachFromReviewDeadline(ctx, service.checkCompletionTimeout)
	defer cancel()
	var keys []ProviderModelKey
	for _, attempt := range summary.Usage.ProviderAttempts {
		key := ProviderModelKey{ProviderID: attempt.Status.ProviderID, RequestedModel: attempt.Status.Model}
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	periods, err := loader.LoadProviderPeriodUsage(loadContext, keys, service.now())
	if err != nil {
		gklog.L(ctx).WarnContext(ctx, "Provider period usage is unavailable", "err", err)
	}
	summary.Usage.PeriodUsage = periods
	summary.Usage.ProviderColumns = columns
	if recorder, found := ctx.Value(usageRecorderContextKey{}).(*UsageRecorder); found && recorder != nil {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		recorder.periodLoaded = true
		recorder.periodVersion = recorder.totals.Requests
		recorder.totals.PeriodUsage = slices.Clone(periods)
	}
}

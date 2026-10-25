package openai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/review"
)

// LoadProviderPeriodUsage reads existing provider and requested-model quota history once per window.
func (client *Client) LoadProviderPeriodUsage(ctx context.Context, keys []review.ProviderModelKey, at time.Time) ([]review.ProviderPeriodUsage, error) {
	ctx, cancel := context.WithTimeout(ctx, budgetRequestTimeout)
	defer cancel()
	if len(keys) == 0 {
		for _, target := range client.providers {
			keys = append(keys, review.ProviderModelKey{ProviderID: target.id, RequestedModel: target.model})
		}
	}
	var results []review.ProviderPeriodUsage
	var failures []error
	seen := make(map[review.ProviderModelKey]bool)
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		var target provider
		index := slices.IndexFunc(client.providers, func(target provider) bool { return target.id == key.ProviderID })
		if index >= 0 {
			target = client.providers[index]
		}
		windows := providerUsageWindows(target)
		if len(windows) == 0 {
			var emptyWindow quota.Window
			windows = append(windows, emptyWindow)
		}
		for _, window := range windows {
			var result review.ProviderPeriodUsage
			result.ProviderID, result.RequestedModel, result.AsOfMS = key.ProviderID, key.RequestedModel, at.UnixMilli()
			result.Window = window
			if window.Mode != "" && client.budgetURL != "" && len(client.budgetSigningKey) > 0 {
				var err error
				result, err = client.readProviderPeriod(ctx, target, result)
				if err != nil {
					failures = append(failures, fmt.Errorf("provider %s model %s period usage: %w", key.ProviderID, key.RequestedModel, err))
				}
			}
			results = append(results, result)
		}
	}
	failure := errors.Join(failures...)
	if failure != nil {
		gklog.L(ctx).WarnContext(ctx, "Read provider period usage failed", "err", failure)
	}
	return results, failure
}

func (client *Client) readProviderPeriod(ctx context.Context, target provider, result review.ProviderPeriodUsage) (review.ProviderPeriodUsage, error) {
	logger := gklog.L(ctx)
	bounds, err := result.Window.Bounds(time.UnixMilli(result.AsOfMS))
	if err != nil {
		logger.WarnContext(ctx, "Provider period window failed", slog.String("err", err.Error()))
		return result, fmt.Errorf("calculate provider period window: %w", err)
	}
	result.Bounds = bounds
	if result.AsOfMS < bounds.EndMS {
		bounds.EndMS, bounds.IncludeEnd = result.AsOfMS, true
	}
	storage := quota.NewClient(client.budgetURL, client.budgetSigningKey, client.httpClient)
	history, err := storage.Query(ctx, result.ProviderID, result.RequestedModel, bounds)
	if err != nil {
		logger.WarnContext(ctx, "Provider period history query failed", slog.String("err", err.Error()))
		return result, fmt.Errorf("read provider period history: %w", err)
	}
	result.HistoryStartMS = history.HistoryStartMS
	result.HistoryComplete = result.Bounds.StartMS >= history.HistoryStartMS
	for _, event := range history.Events {
		if event.TotalTokens > quota.MaximumInteger-result.TotalTokens {
			return result, errors.New("provider period usage exceeds the accounting integer range")
		}
		result.TotalTokens += event.TotalTokens
		result.ReportedRequests++
		cost, priced := periodEventCost(target, result.RequestedModel, event.Usage)
		if priced {
			result.PricedRequests++
			result.EstimatedCostUSD += cost
		}
	}
	result.Available = true
	result.CostComplete = result.PricedRequests == result.ReportedRequests
	return result, nil
}

func periodEventCost(target provider, requestedModel string, usage quota.Usage) (float64, bool) {
	model := requestedModel
	if usage.SDKUsage != nil && usage.SDKUsage.Model != "" {
		model = usage.SDKUsage.Model
	}
	pricing, found := config.FindModelPricing(target.pricingByModel, model)
	if !found || (usage.InputTokens > 0 && (usage.CachedInputTokens == nil || usage.CacheWriteTokens == nil)) {
		return 0, false
	}
	var cached, writes int64
	if usage.CachedInputTokens != nil {
		cached = *usage.CachedInputTokens
	}
	if usage.CacheWriteTokens != nil {
		writes = *usage.CacheWriteTokens
	}
	cost, known := pricing.Estimate(usage.InputTokens, cached, writes, usage.OutputTokens, usage.ServiceTier)
	return cost.TotalUSD(), known
}

func providerUsageWindows(target provider) []quota.Window {
	var windows []quota.Window
	if target.tokenWindow.Mode != "" {
		windows = append(windows, target.tokenWindow)
	} else if target.dailyTokenLimit > 0 {
		windows = append(windows, quota.Window{Mode: quota.Cron, Duration: "", Anchor: "", Schedule: "0 0 * * *", Timezone: "UTC"})
	}
	for _, limit := range target.tokenLimits {
		if limit.Limit > 0 && !slices.Contains(windows, limit.Window) {
			windows = append(windows, limit.Window)
		}
	}
	return windows
}

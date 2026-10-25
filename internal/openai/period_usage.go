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

// LoadProviderPeriodUsage reads all stored model usage for each provider and associates it with report rows.
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
	type periodKey struct {
		providerID string
		window     quota.Window
	}
	byPeriod := make(map[periodKey]review.ProviderPeriodUsage)
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		index := slices.IndexFunc(client.providers, func(target provider) bool { return target.id == key.ProviderID })
		var target provider
		if index >= 0 {
			target = client.providers[index]
		}
		windows := providerUsageWindows(target)
		if len(windows) == 0 {
			var emptyWindow quota.Window
			windows = []quota.Window{emptyWindow}
		}
		for _, window := range windows {
			cacheKey := periodKey{providerID: key.ProviderID, window: window}
			if cached, exists := byPeriod[cacheKey]; exists {
				cached.RequestedModel = key.RequestedModel
				results = append(results, cached)
				continue
			}
			var result review.ProviderPeriodUsage
			result.ProviderID, result.RequestedModel, result.AsOfMS = key.ProviderID, key.RequestedModel, at.UnixMilli()
			result.Window = window
			var err error
			if window.Mode != "" {
				result.Bounds, err = window.Bounds(at)
				if err == nil && client.budgetURL != "" && len(client.budgetSigningKey) > 0 && !review.ProviderUsageDeliveryFailed(ctx, key.ProviderID) {
					result, err = client.readProviderPeriod(ctx, target, result)
				}
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("provider %s period usage: %w", key.ProviderID, err))
			}
			byPeriod[cacheKey] = result
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
	storage := quota.NewClient(client.budgetURL, client.budgetSigningKey, client.httpClient)
	queryBounds := result.Bounds
	if result.AsOfMS < queryBounds.EndMS {
		queryBounds.EndMS = result.AsOfMS
		queryBounds.IncludeEnd = true
	}
	totals, err := storage.SummarizeProvider(ctx, result.ProviderID, queryBounds)
	if err != nil {
		logger.WarnContext(ctx, "Provider period totals query failed", slog.String("err", err.Error()))
		return result, fmt.Errorf("read provider period totals: %w", err)
	}
	result.HistoryStartMS = totals.HistoryStartMS
	result.HistoryComplete = totals.HistoryStartMS > 0 && result.Bounds.StartMS >= totals.HistoryStartMS
	for _, group := range totals.Groups {
		if group.TotalTokens > quota.MaximumInteger-result.TotalTokens || group.Requests > quota.MaximumInteger-int64(result.ReportedRequests) {
			return result, errors.New("provider period usage exceeds the accounting integer range")
		}
		result.TotalTokens += group.TotalTokens
		result.ReportedRequests += int(group.Requests)
		model := group.ReportedModel
		if model == "" {
			model = group.RequestedModel
		}
		pricing, found := config.FindModelPricing(target.pricingByModel, model)
		cost, known := pricing.Estimate(group.InputTokens, group.CachedInputTokens, group.CacheWriteTokens, group.OutputTokens, group.ServiceTier)
		if found && known && group.CacheDetailsKnown {
			result.PricedRequests += int(group.Requests)
			result.EstimatedCostUSD += cost.TotalUSD()
		}
	}
	result.CostComplete = result.PricedRequests == result.ReportedRequests
	result.Available = true
	return result, nil
}

func providerUsageWindows(target provider) []quota.Window {
	var windows []quota.Window
	if target.tokenWindow.Mode != "" {
		windows = append(windows, target.tokenWindow)
	} else if target.dailyTokenLimit > 0 {
		windows = append(windows, quota.Window{Mode: quota.Cron, Duration: "", Anchor: "", Schedule: "0 0 * * *", Timezone: "UTC"})
	}
	for _, limit := range target.tokenLimits {
		if !slices.Contains(windows, limit.Window) {
			windows = append(windows, limit.Window)
		}
	}
	return windows
}

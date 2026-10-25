package quota

import (
	"context"
	"errors"
)

type summaryRequest struct {
	Action       string `json:"action"`
	ProviderID   string `json:"provider_id"`
	StartMS      int64  `json:"start_ms"`
	EndMS        int64  `json:"end_ms"`
	IncludeStart bool   `json:"include_start"`
	IncludeEnd   bool   `json:"include_end"`
}

// UsageGroup combines reports that use the same model, tier, and cache detail availability.
type UsageGroup struct {
	RequestedModel    string `json:"requested_model"`
	ReportedModel     string `json:"reported_model"`
	ServiceTier       string `json:"service_tier"`
	Requests          int64  `json:"requests"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	TotalTokens       int64  `json:"total_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	CacheDetailsKnown bool   `json:"cache_details_known"`
}

// UsageTotals returns bounded accounting groups without native SDK payloads.
type UsageTotals struct {
	Groups           []UsageGroup `json:"groups"`
	HistoryStartMS   int64        `json:"history_start_ms"`
	SnapshotSequence int64        `json:"snapshot_sequence"`
}

// SummarizeProvider reads all requested models for one provider using a bounded response.
func (client *Client) SummarizeProvider(ctx context.Context, providerID string, bounds Bounds) (UsageTotals, error) {
	var result UsageTotals
	request := summaryRequest{
		Action: "summarize_usage", ProviderID: providerID,
		StartMS: bounds.StartMS, EndMS: bounds.EndMS, IncludeStart: bounds.IncludeStart, IncludeEnd: bounds.IncludeEnd,
	}
	if err := post(ctx, client, request, &result); err != nil {
		return UsageTotals{}, err
	}
	if result.HistoryStartMS <= 0 || result.SnapshotSequence < 0 || len(result.Groups) > maximumPageSize {
		return UsageTotals{}, errors.New("accounting service returned invalid summary metadata")
	}
	for _, group := range result.Groups {
		cached, writes := group.CachedInputTokens, group.CacheWriteTokens
		usage := Usage{
			InputTokens: group.InputTokens, OutputTokens: group.OutputTokens, TotalTokens: group.TotalTokens,
			CachedInputTokens: &cached, CacheWriteTokens: &writes, ServiceTier: group.ServiceTier, SDKUsage: nil,
		}
		if group.Requests <= 0 || group.Requests > MaximumInteger || !validUsage(usage) {
			return UsageTotals{}, errors.New("accounting service returned invalid summary usage")
		}
	}
	return result, nil
}

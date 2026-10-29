package quota

import (
	"context"
	"errors"
)

// UsageTotalsGroup groups the counters that share the same pricing inputs.
type UsageTotalsGroup struct {
	ReportedModel     string `json:"reported_model"`
	Requests          int64  `json:"requests"`
	FirstAdmittedAtMS int64  `json:"first_admitted_at_ms"`
	Usage
}

// TotalsResponse retains the recorded history boundary and sequence snapshot.
type TotalsResponse struct {
	Groups           []UsageTotalsGroup `json:"groups"`
	SnapshotSequence int64              `json:"snapshot_sequence"`
	HistoryStartMS   int64              `json:"history_start_ms"`
}

type totalsRequest struct {
	Action           string `json:"action"`
	ProviderID       string `json:"provider_id"`
	Model            string `json:"model"`
	StartMS          int64  `json:"start_ms"`
	EndMS            int64  `json:"end_ms"`
	IncludeStart     bool   `json:"include_start"`
	IncludeEnd       bool   `json:"include_end"`
	AfterGroup       int64  `json:"after_group"`
	SnapshotSequence int64  `json:"snapshot_sequence"`
	PageSize         int    `json:"page_size"`
}

type totalsPage struct {
	TotalsResponse
	NextGroup int64 `json:"next_group"`
}

// QueryTotals reads grouped pricing inputs without downloading native SDK envelopes.
func (client *Client) QueryTotals(ctx context.Context, providerID, requestedModel string, bounds Bounds) (TotalsResponse, error) {
	request := totalsRequest{
		Action: "query_usage_totals", ProviderID: providerID, Model: requestedModel,
		StartMS: bounds.StartMS, EndMS: bounds.EndMS, IncludeStart: bounds.IncludeStart,
		IncludeEnd: bounds.IncludeEnd, AfterGroup: 0, SnapshotSequence: 0, PageSize: maximumPageSize,
	}
	var result TotalsResponse
	first := true
	for {
		var page totalsPage
		if err := post(ctx, client, request, &page); err != nil {
			return TotalsResponse{}, err
		}
		if !validTotalsMetadata(page, maximumPageSize) {
			return TotalsResponse{}, errors.New("accounting service returned invalid totals metadata")
		}
		if !first && (page.SnapshotSequence != result.SnapshotSequence || page.HistoryStartMS != result.HistoryStartMS) {
			return TotalsResponse{}, errors.New("accounting service changed the usage totals snapshot")
		}
		result.SnapshotSequence, result.HistoryStartMS = page.SnapshotSequence, page.HistoryStartMS
		for _, group := range page.Groups {
			if !validTotalsGroup(group, page.SnapshotSequence, bounds) ||
				(len(result.Groups) > 0 && !orderedTotalsGroups(result.Groups[len(result.Groups)-1], group)) {
				return TotalsResponse{}, errors.New("accounting service returned invalid usage totals")
			}
			result.Groups = append(result.Groups, group)
		}
		if page.NextGroup == 0 {
			return result, nil
		}
		if len(page.Groups) == 0 || page.NextGroup != request.AfterGroup+int64(len(page.Groups)) || page.NextGroup <= request.AfterGroup {
			return TotalsResponse{}, errors.New("accounting service returned a nonadvancing totals cursor")
		}
		request.AfterGroup, request.SnapshotSequence = page.NextGroup, page.SnapshotSequence
		first = false
	}
}

func validTotalsMetadata(page totalsPage, maximumGroups int) bool {
	return page.SnapshotSequence >= 0 && page.SnapshotSequence <= MaximumInteger &&
		page.NextGroup >= 0 && page.NextGroup <= page.SnapshotSequence &&
		page.HistoryStartMS > 0 && page.HistoryStartMS <= MaximumInteger && len(page.Groups) <= maximumGroups
}

func validTotalsGroup(group UsageTotalsGroup, snapshot int64, bounds Bounds) bool {
	return group.ReportedModel != "" && group.Requests > 0 && group.Requests <= snapshot &&
		group.FirstAdmittedAtMS >= 0 && group.FirstAdmittedAtMS <= MaximumInteger &&
		bounds.contains(group.FirstAdmittedAtMS) && group.SDKUsage == nil && validUsage(group.Usage)
}

func orderedTotalsGroups(previous, current UsageTotalsGroup) bool {
	if previous.ReportedModel != current.ReportedModel {
		return previous.ReportedModel < current.ReportedModel
	}
	if previous.ServiceTier != current.ServiceTier {
		return previous.ServiceTier < current.ServiceTier
	}
	if (previous.CachedInputTokens == nil) != (current.CachedInputTokens == nil) {
		return previous.CachedInputTokens == nil
	}
	if (previous.CacheWriteTokens == nil) != (current.CacheWriteTokens == nil) {
		return previous.CacheWriteTokens == nil
	}
	previousWrites := previous.CacheWriteTokens != nil && *previous.CacheWriteTokens > 0
	currentWrites := current.CacheWriteTokens != nil && *current.CacheWriteTokens > 0
	if previousWrites != currentWrites {
		return !previousWrites
	}
	previousReady := previous.InputTokens == 0 || (previous.CachedInputTokens != nil && previous.CacheWriteTokens != nil)
	currentReady := current.InputTokens == 0 || (current.CachedInputTokens != nil && current.CacheWriteTokens != nil)
	if previousReady != currentReady {
		return !previousReady
	}
	return false
}

package review

import (
	"context"
	"sync"
)

// ModelUsage records one model request and any usage the provider reported.
// The same shape represents one request and one model's aggregate.
type ModelUsage struct {
	RequestedModel              string
	Model                       string
	Priced                      bool
	Requests                    int
	ReportedRequests            int
	InputTokens                 int64
	CachedInputTokens           int64
	AudioInputTokens            int64
	OutputTokens                int64
	ReasoningTokens             int64
	AudioOutputTokens           int64
	AcceptedPredictionTokens    int64
	RejectedPredictionTokens    int64
	TotalTokens                 int64
	EstimatedInputCostUSD       float64
	EstimatedCachedInputCostUSD float64
	EstimatedOutputCostUSD      float64
	EstimatedCostUSD            float64
}

// UsageSummary aggregates every attempted model request in one review run.
type UsageSummary struct {
	Requests                    int
	ReportedRequests            int
	PricedRequests              int
	InputTokens                 int64
	CachedInputTokens           int64
	AudioInputTokens            int64
	OutputTokens                int64
	ReasoningTokens             int64
	AudioOutputTokens           int64
	AcceptedPredictionTokens    int64
	RejectedPredictionTokens    int64
	TotalTokens                 int64
	EstimatedInputCostUSD       float64
	EstimatedCachedInputCostUSD float64
	EstimatedOutputCostUSD      float64
	EstimatedCostUSD            float64
	Models                      []ModelUsage
}

type usageRecorderContextKey struct{}

type usageModelKey struct {
	requestedModel string
	model          string
	priced         bool
}

// UsageRecorder safely aggregates concurrent model requests in one review run.
type UsageRecorder struct {
	mu      sync.Mutex
	totals  UsageSummary
	models  map[usageModelKey]ModelUsage
	ordered []usageModelKey
}

// WithUsageRecorder attaches a new usage recorder to a context.
func WithUsageRecorder(ctx context.Context) (context.Context, *UsageRecorder) {
	var totals UsageSummary
	recorder := &UsageRecorder{
		mu:      sync.Mutex{},
		totals:  totals,
		models:  make(map[usageModelKey]ModelUsage),
		ordered: nil,
	}
	return context.WithValue(ctx, usageRecorderContextKey{}, recorder), recorder
}

// RecordModelUsage adds one attempted model request to the context recorder.
// It does nothing when the caller did not attach a recorder.
func RecordModelUsage(ctx context.Context, usage ModelUsage) {
	recorder, ok := ctx.Value(usageRecorderContextKey{}).(*UsageRecorder)
	if !ok || recorder == nil {
		return
	}
	recorder.record(usage)
}

// UsageFromContext returns the current usage snapshot for a context.
func UsageFromContext(ctx context.Context) UsageSummary {
	recorder, ok := ctx.Value(usageRecorderContextKey{}).(*UsageRecorder)
	if !ok || recorder == nil {
		var summary UsageSummary
		return summary
	}
	return recorder.Summary()
}

func (recorder *UsageRecorder) record(usage ModelUsage) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	if usage.Requests == 0 {
		usage.Requests = 1
	}
	addModelUsage(&recorder.totals, usage)
	key := usageModelKey{
		requestedModel: usage.RequestedModel,
		model:          usage.Model,
		priced:         usage.Priced,
	}
	model, found := recorder.models[key]
	if !found {
		model.RequestedModel = usage.RequestedModel
		model.Model = usage.Model
		model.Priced = usage.Priced
		recorder.ordered = append(recorder.ordered, key)
	}
	addUsage(&model, usage)
	recorder.models[key] = model
}

// Summary returns a stable copy of all usage recorded so far.
func (recorder *UsageRecorder) Summary() UsageSummary {
	if recorder == nil {
		var summary UsageSummary
		return summary
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	summary := recorder.totals
	summary.Models = make([]ModelUsage, 0, len(recorder.ordered))
	for _, key := range recorder.ordered {
		summary.Models = append(summary.Models, recorder.models[key])
	}
	return summary
}

func addModelUsage(summary *UsageSummary, usage ModelUsage) {
	summary.Requests += usage.Requests
	if usage.Priced {
		summary.PricedRequests += usage.Requests
	}
	summary.ReportedRequests += usage.ReportedRequests
	summary.InputTokens += usage.InputTokens
	summary.CachedInputTokens += usage.CachedInputTokens
	summary.AudioInputTokens += usage.AudioInputTokens
	summary.OutputTokens += usage.OutputTokens
	summary.ReasoningTokens += usage.ReasoningTokens
	summary.AudioOutputTokens += usage.AudioOutputTokens
	summary.AcceptedPredictionTokens += usage.AcceptedPredictionTokens
	summary.RejectedPredictionTokens += usage.RejectedPredictionTokens
	summary.TotalTokens += usage.TotalTokens
	summary.EstimatedInputCostUSD += usage.EstimatedInputCostUSD
	summary.EstimatedCachedInputCostUSD += usage.EstimatedCachedInputCostUSD
	summary.EstimatedOutputCostUSD += usage.EstimatedOutputCostUSD
	summary.EstimatedCostUSD += usage.EstimatedCostUSD
}

func addUsage(total *ModelUsage, usage ModelUsage) {
	total.Requests += usage.Requests
	total.ReportedRequests += usage.ReportedRequests
	total.InputTokens += usage.InputTokens
	total.CachedInputTokens += usage.CachedInputTokens
	total.AudioInputTokens += usage.AudioInputTokens
	total.OutputTokens += usage.OutputTokens
	total.ReasoningTokens += usage.ReasoningTokens
	total.AudioOutputTokens += usage.AudioOutputTokens
	total.AcceptedPredictionTokens += usage.AcceptedPredictionTokens
	total.RejectedPredictionTokens += usage.RejectedPredictionTokens
	total.TotalTokens += usage.TotalTokens
	total.EstimatedInputCostUSD += usage.EstimatedInputCostUSD
	total.EstimatedCachedInputCostUSD += usage.EstimatedCachedInputCostUSD
	total.EstimatedOutputCostUSD += usage.EstimatedOutputCostUSD
	total.EstimatedCostUSD += usage.EstimatedCostUSD
}

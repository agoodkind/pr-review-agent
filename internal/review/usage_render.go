package review

import (
	"fmt"
	"strings"
)

func renderUsageDetails(usage UsageSummary) string {
	var builder strings.Builder
	builder.WriteString("\n#### Model usage\n\n| | |\n| --- | --- |\n")
	if usage.ReportedRequests == 0 {
		fmt.Fprintf(&builder, "| Requests | `%d` |\n\nToken usage was not reported.\n", usage.Requests)
		return builder.String()
	}
	rows := []struct {
		name  string
		value int64
	}{
		{"Requests", int64(usage.Requests)},
		{"Requests reporting usage", int64(usage.ReportedRequests)},
		{"Input tokens", usage.InputTokens},
		{"Cached input tokens", usage.CachedInputTokens},
		{"Audio input tokens", usage.AudioInputTokens},
		{"Output tokens", usage.OutputTokens},
		{"Reasoning tokens", usage.ReasoningTokens},
		{"Audio output tokens", usage.AudioOutputTokens},
		{"Accepted prediction tokens", usage.AcceptedPredictionTokens},
		{"Rejected prediction tokens", usage.RejectedPredictionTokens},
		{"Total tokens", usage.TotalTokens},
	}
	for _, row := range rows {
		fmt.Fprintf(&builder, "| %s | `%d` |\n", row.name, row.value)
	}
	fmt.Fprintf(&builder, "| Estimated input cost | $%.8f |\n", usage.EstimatedInputCostUSD)
	fmt.Fprintf(&builder, "| Estimated cached input cost | $%.8f |\n", usage.EstimatedCachedInputCostUSD)
	fmt.Fprintf(&builder, "| Estimated output cost | $%.8f |\n", usage.EstimatedOutputCostUSD)
	fmt.Fprintf(&builder, "| Estimated total cost | $%.8f |\n", usage.EstimatedCostUSD)
	fmt.Fprintf(&builder, "| Requests with known pricing | `%d` of `%d` |\n", usage.PricedRequests, usage.Requests)
	if usage.PricedRequests < usage.Requests {
		builder.WriteString("\nSome requests have no known pricing. The estimate excludes their cost.\n")
	}
	if len(usage.Models) > 0 {
		builder.WriteString("\n| Requested model | Reported model | Requests | Input | Cached input | Output | Total | Estimated cost |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n")
		for _, model := range usage.Models {
			cost := "unknown"
			if model.Priced {
				cost = fmt.Sprintf("$%.8f", model.EstimatedCostUSD)
			}
			fmt.Fprintf(&builder, "| %s | %s | %d | %d | %d | %d | %d | %s |\n",
				usageModelCell(model.RequestedModel), usageModelCell(model.Model), model.Requests,
				model.InputTokens, model.CachedInputTokens, model.OutputTokens, model.TotalTokens, cost)
		}
	}
	return builder.String()
}

func usageModelCell(model string) string {
	return strings.ReplaceAll(codeSpan(model), "|", "&#124;")
}

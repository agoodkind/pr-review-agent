package review

import (
	"fmt"
	"strconv"
	"strings"

	outputtemplates "goodkind.io/pr-review-agent/config/templates"
)

const rawNull = "null"

func renderRawDetails(summary Summary) string {
	usage := summary.Usage
	var builder strings.Builder
	builder.WriteString(
		"<details>\n<summary>Review details</summary>\n\n| key | value |\n| --- | --- |\n",
	)
	writeRawDetail(&builder, "head", shortHead(summary.Head))
	fmt.Fprintf(&builder, "| `result` | %s |\n", rawResultCell(summary.Result))
	fmt.Fprintf(&builder, "| `providers` | %s |\n", rawProviderUsage(usage))

	rows := []struct {
		key   string
		value int64
	}{
		{"requests", int64(usage.Requests)},
		{"reported_requests", int64(usage.ReportedRequests)},
		{"input_tokens", usage.InputTokens},
		{"cached_input_tokens", usage.CachedInputTokens},
		{"cache_write_tokens", usage.CacheWriteTokens},
		{"audio_input_tokens", usage.AudioInputTokens},
		{"output_tokens", usage.OutputTokens},
		{"reasoning_tokens", usage.ReasoningTokens},
		{"audio_output_tokens", usage.AudioOutputTokens},
		{"accepted_prediction_tokens", usage.AcceptedPredictionTokens},
		{"rejected_prediction_tokens", usage.RejectedPredictionTokens},
		{"total_tokens", usage.TotalTokens},
	}
	for _, row := range rows {
		writeRawDetail(&builder, row.key, strconv.FormatInt(row.value, 10))
	}

	if len(usage.ServiceTiers) > 0 {
		cells := make([]string, 0, len(usage.ServiceTiers))
		for _, tier := range usage.ServiceTiers {
			cells = append(cells, rawTableCell(tier))
		}

		fmt.Fprintf(&builder, "| `service_tiers` | %s |\n", strings.Join(cells, ", "))
	}

	costs := []struct {
		key   string
		value float64
	}{
		{"estimated_input_cost_usd", usage.EstimatedInputCostUSD},
		{"estimated_cached_input_cost_usd", usage.EstimatedCachedInputCostUSD},
		{"estimated_cache_write_cost_usd", usage.EstimatedCacheWriteCostUSD},
		{"estimated_output_cost_usd", usage.EstimatedOutputCostUSD},
		{"estimated_cost_usd", usage.EstimatedCostUSD},
	}
	for _, cost := range costs {
		value := rawNull
		if usage.PricedRequests > 0 {
			value = fmt.Sprintf("%.8f", cost.value)
			if cost.key == "estimated_cache_write_cost_usd" &&
				hasUnpricedCacheWrites(usage) {
				value = rawNull
			}
		}

		writeRawDetail(&builder, cost.key, value)
	}

	writeRawDetail(&builder, "priced_requests", strconv.Itoa(usage.PricedRequests))
	builder.WriteString("\n</details>")

	return builder.String()
}

func writeRawDetail(builder *strings.Builder, key, value string) {
	fmt.Fprintf(builder, "| %s | %s |\n", rawTableCell(key), rawTableCell(value))
}

func rawResultCell(result string) string {
	if result == "" {
		return rawTableCell(rawNull)
	}

	if !strings.ContainsAny(result, "\r\n") {
		return rawTableCell(result)
	}

	var escaped strings.Builder

	for _, character := range result {
		if strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~\r\n", character) {
			fmt.Fprintf(&escaped, "&#%d;", character)

			continue
		}

		escaped.WriteRune(character)
	}

	return strings.ReplaceAll(
		strings.TrimSuffix(outputtemplates.RawResult, "\n"),
		"{{.Input}}",
		escaped.String(),
	)
}

func rawProviderUsage(usage UsageSummary) string {
	providers := make([]string, 0, len(usage.ProviderAttempts))
	attempts := make(map[string]ProviderAttempt)
	totals := make(map[string]ModelUsage)

	for _, attempt := range usage.ProviderAttempts {
		provider := attempt.Status.ProviderID
		previous, exists := attempts[provider]
		if !exists {
			providers = append(providers, provider)
		}

		if !exists || attempt.Sequence >= previous.Sequence {
			attempts[provider] = attempt
		}
	}

	for _, model := range usage.Models {
		if _, exists := totals[model.ProviderID]; !exists {
			if _, attempted := attempts[model.ProviderID]; !attempted {
				providers = append(providers, model.ProviderID)
			}
		}

		total := totals[model.ProviderID]
		addUsage(&total, model)
		totals[model.ProviderID] = total
	}

	cells := make([]string, 0, len(providers))
	for _, provider := range providers {
		total := totals[provider]
		attempt := attempts[provider]
		value := rawNull
		switch {
		case total.ReportedRequests > 0:
			value = strconv.FormatInt(total.TotalTokens, 10)
		case attempt.ErrorCode != "":
			value = attempt.ErrorCode
		case attempt.Status.Cause != "":
			value = string(attempt.Status.Cause)
		}

		cells = append(cells, rawTableCell(provider)+": "+rawTableCell(value))
	}

	if len(cells) == 0 {
		return rawTableCell(rawNull)
	}

	return strings.Join(cells, "<br>")
}

func hasUnpricedCacheWrites(usage UsageSummary) bool {
	for _, model := range usage.Models {
		if !model.Priced && model.CacheWriteTokens > 0 {
			return true
		}
	}

	return false
}

func usageModelCell(model string) string {
	return strings.ReplaceAll(codeSpan(model), "|", "&#124;")
}

func rawTableCell(value string) string {
	value = strings.ReplaceAll(replyLineBreaks.Replace(value), "\n", " ")
	value = strings.ReplaceAll(value, "|", "\\|")

	delimiter := "`"
	for strings.Contains(value, delimiter) {
		delimiter += "`"
	}

	if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") ||
		(strings.HasPrefix(
			value,
			" ",
		) && strings.HasSuffix(value, " ") && strings.TrimSpace(value) != "") {
		value = " " + value + " "
	}

	return delimiter + value + delimiter
}

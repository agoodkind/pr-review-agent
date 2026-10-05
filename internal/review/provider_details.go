package review

import (
	"fmt"
	"slices"
	"strings"

	"goodkind.io/pr-review-agent/internal/reviewrules"
)

type providerDetailRow struct {
	attempt  ProviderAttempt
	attempts []ProviderAttempt
	usage    ModelUsage
	models   []string
	priced   int
}

func renderProviderDetails(summary UsageSummary) string {
	if len(summary.ProviderAttempts) == 0 || len(summary.ProviderColumns) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("\n#### Provider details\n\n|")
	for _, column := range summary.ProviderColumns {
		fmt.Fprintf(&builder, " %s |", providerColumnHeading(column))
	}
	builder.WriteString("\n|")
	for range summary.ProviderColumns {
		builder.WriteString(" --- |")
	}
	builder.WriteString("\n")
	for _, row := range providerDetailRows(summary) {
		builder.WriteString("|")
		for _, column := range summary.ProviderColumns {
			fmt.Fprintf(&builder, " %s |", providerColumnValue(column, row))
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

func providerDetailRows(summary UsageSummary) []providerDetailRow {
	var rows []providerDetailRow
	for _, attempt := range summary.ProviderAttempts {
		index := slices.IndexFunc(rows, func(row providerDetailRow) bool {
			return row.attempt.Status.ProviderID == attempt.Status.ProviderID && row.attempt.Status.Model == attempt.Status.Model
		})
		if index < 0 {
			var usage ModelUsage
			rows = append(rows, providerDetailRow{attempt: attempt, attempts: nil, usage: usage, models: nil, priced: 0})
			index = len(rows) - 1
		}
		if attempt.Sequence > rows[index].attempt.Sequence {
			rows[index].attempt = attempt
		}
		rows[index].attempts = append(rows[index].attempts, attempt)
	}
	for index := range rows {
		row := &rows[index]
		for _, model := range summary.Models {
			if model.ProviderID != row.attempt.Status.ProviderID || model.RequestedModel != row.attempt.Status.Model {
				continue
			}
			addUsage(&row.usage, model)
			if model.Priced {
				row.priced += model.ReportedRequests
			}
			if model.Model != "" && !slices.Contains(row.models, model.Model) {
				row.models = append(row.models, model.Model)
			}
		}
	}
	return rows
}

func providerColumnHeading(column reviewrules.ProviderDetailColumn) string {
	switch column {
	case reviewrules.ProviderIdentityColumn:
		return "Provider"
	case reviewrules.ProviderModelsColumn:
		return "Requested / reported models"
	case reviewrules.ProviderSettingsColumn:
		return "API / configured reasoning / output cap"
	case reviewrules.ProviderRequestsColumn:
		return "Attempts / API requests / completions"
	case reviewrules.ProviderTokensColumn:
		return "Reported tokens"
	case reviewrules.ProviderCostColumn:
		return "Estimated cost"
	case reviewrules.ProviderQuotaColumn:
		return "App quota before last attempt"
	case reviewrules.ProviderResultColumn:
		return "Result"
	}
	return ""
}

func providerColumnValue(column reviewrules.ProviderDetailColumn, row providerDetailRow) string {
	attempt := row.attempt
	switch column {
	case reviewrules.ProviderIdentityColumn:
		return usageModelCell(attempt.Status.ProviderID)
	case reviewrules.ProviderModelsColumn:
		models := "not reported"
		if len(row.models) > 0 {
			cells := make([]string, 0, len(row.models))
			for _, model := range row.models {
				cells = append(cells, usageModelCell(model))
			}
			models = strings.Join(cells, ", ")
		}
		return usageModelCell(attempt.Status.Model) + " / " + models
	case reviewrules.ProviderSettingsColumn:
		outputLimit := "not sent"
		if !attempt.OutputCapOmitted {
			outputLimit = formatTokenCount(attempt.MaxOutputTokens)
		}
		return usageModelCell(string(attempt.API)) + " / " + usageModelCell(string(attempt.ReasoningEffort)) + " / " + outputLimit
	case reviewrules.ProviderRequestsColumn:
		var attempts, completed int
		for _, outcome := range row.attempts {
			attempts += outcome.Count
			if outcome.Completed {
				completed += outcome.Count
			}
		}
		return fmt.Sprintf("%d / %d / %d", attempts, row.usage.Requests, completed)
	case reviewrules.ProviderTokensColumn:
		if row.usage.ReportedRequests == 0 {
			return "not reported"
		}
		return fmt.Sprintf("Input %s<br>Cached %s<br>Output %s<br>Reasoning %s<br>Total %s<br>Usage reports %d / %d",
			formatTokenCount(row.usage.InputTokens), formatTokenCount(row.usage.CachedInputTokens),
			formatTokenCount(row.usage.OutputTokens), formatTokenCount(row.usage.ReasoningTokens),
			formatTokenCount(row.usage.TotalTokens), row.usage.ReportedRequests, row.usage.Requests)
	case reviewrules.ProviderCostColumn:
		if row.priced == 0 {
			return "unknown"
		}
		return fmt.Sprintf("$%.8f<br>Priced reports %d / %d", row.usage.EstimatedCostUSD, row.priced, row.usage.ReportedRequests)
	case reviewrules.ProviderQuotaColumn:
		return providerQuotaCell(attempt.Status)
	case reviewrules.ProviderResultColumn:
		return providerResultCell(row.attempts)
	}
	return ""
}

func providerQuotaCell(status ProviderStatus) string {
	if status.Limit == 0 {
		return "No app limit"
	}
	if !status.QuotaKnown {
		return "Unknown / " + formatTokenCount(status.Limit)
	}
	label := fmt.Sprintf("%s / %s (%s left)", formatTokenCount(status.Used), formatTokenCount(status.Limit), formatTokenCount(status.Remaining))
	window := status.TokenWindow
	if window.Mode != "" {
		label += "<br>" + usageModelCell(string(window.Mode))
		if window.Duration != "" {
			label += " " + usageModelCell(window.Duration)
		}
		if window.Schedule != "" {
			label += " " + usageModelCell(window.Schedule) + " " + usageModelCell(window.Timezone)
		}
	}
	return label
}

func providerResultCell(attempts []ProviderAttempt) string {
	results := make([]string, 0, len(attempts))
	var fallbacks int
	for _, attempt := range attempts {
		label := providerCauseLabel(attempt.Status.Cause)
		if attempt.HTTPStatus != 0 {
			label += fmt.Sprintf(" (HTTP %d)", attempt.HTTPStatus)
		}
		if attempt.ErrorCode != "" {
			label += " " + usageModelCell(attempt.ErrorCode)
		}
		results = append(results, fmt.Sprintf("%s: %d", label, attempt.Count))
		if attempt.Fallback {
			fallbacks += attempt.Count
		}
	}
	if fallbacks > 0 {
		results = append(results, fmt.Sprintf("Next provider selected: %d", fallbacks))
	}
	return strings.Join(results, "<br>")
}

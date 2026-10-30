package review

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/quota"
)

func providerPeriodUsageCell(periods []ProviderPeriodUsage) string {
	if len(periods) == 0 {
		return "unavailable"
	}
	blocks := make([]string, 0, len(periods))
	for _, period := range periods {
		label := providerPeriodWindowLabel(period.Window)
		if !period.Available {
			blocks = append(blocks, label+"<br>unavailable")
			continue
		}
		parts := []string{
			label, "**" + compactPeriodTokens(period.TotalTokens) + " tokens**", providerPeriodCost(period),
		}
		if !period.HistoryComplete && period.Window.Mode != "" {
			parts = append(parts, "Partial history")
		}
		if !period.CostComplete && period.PricedRequests > 0 {
			parts = append(parts, "Partial pricing")
		}
		blocks = append(blocks, strings.Join(parts, "<br>"))
	}
	return strings.Join(blocks, "<br><br>")
}

func providerPeriodCost(period ProviderPeriodUsage) string {
	if !period.CostComplete && period.PricedRequests == 0 {
		return "cost unavailable"
	}
	precision := 2
	if period.EstimatedCostUSD > 0 && period.EstimatedCostUSD < 0.01 {
		precision = int(-math.Floor(math.Log10(period.EstimatedCostUSD))) + 1
	}
	return "**~$" + strconv.FormatFloat(period.EstimatedCostUSD, 'f', precision, 64) + " estimated**"
}

func renderProviderPeriodDetails(rows []providerDetailRow) string {
	var builder strings.Builder
	for _, row := range rows {
		if len(row.periods) == 0 {
			continue
		}
		if builder.Len() == 0 {
			builder.WriteString("\n#### Period usage details\n\nUsage timestamps use UTC. Configured anchors retain their original offsets.\n")
		}
		fmt.Fprintf(&builder, "\n##### %s / %s\n", usageModelCell(row.attempt.Status.ProviderID), usageModelCell(row.attempt.Status.Model))
		for _, period := range row.periods {
			writeProviderPeriodDetails(&builder, period)
		}
	}
	return builder.String()
}

func writeProviderPeriodDetails(builder *strings.Builder, period ProviderPeriodUsage) {
	builder.WriteString("\n| Field | Value |\n| --- | --- |\n")
	fmt.Fprintf(builder, "| Window | %s |\n", providerPeriodWindowLabel(period.Window))
	for _, field := range []struct{ label, value string }{
		{label: "Duration", value: period.Window.Duration},
		{label: "Schedule", value: period.Window.Schedule},
		{label: "Timezone", value: period.Window.Timezone},
		{label: "Configured anchor", value: period.Window.Anchor},
	} {
		if field.value != "" {
			fmt.Fprintf(builder, "| %s | %s |\n", field.label, usageModelCell(field.value))
		}
	}
	if !period.Available {
		builder.WriteString("| Availability | unavailable |\n")
		return
	}
	fmt.Fprintf(builder, "| Period start | %s |\n| Period end | %s |\n| Captured | %s |\n",
		periodTimestamp(period.Bounds.StartMS), periodTimestamp(period.Bounds.EndMS), periodTimestamp(period.AsOfMS))
	if !period.HistoryComplete {
		label := "Partial history from"
		if period.Window.Mode == "" {
			label = "Recorded history begins"
		}
		fmt.Fprintf(builder, "| %s | %s |\n", label, periodTimestamp(period.HistoryStartMS))
	}
	fmt.Fprintf(builder, "| Requests with known prices | %s of %s |\n",
		formatTokenCount(int64(period.PricedRequests)), formatTokenCount(int64(period.ReportedRequests)))
}

func providerPeriodWindowLabel(window quota.Window) string {
	label := usageModelCell(string(window.Mode))
	if window.Mode == "" {
		return "Recorded usage"
	}
	if window.Duration != "" {
		label += " " + usageModelCell(window.Duration)
	}
	return label
}

func compactPeriodTokens(tokens int64) string {
	for _, unit := range []struct {
		threshold int64
		suffix    string
	}{
		{threshold: 1_000_000_000, suffix: "b"},
		{threshold: 1_000_000, suffix: "m"},
		{threshold: 1_000, suffix: "k"},
	} {
		if tokens >= unit.threshold {
			value := strconv.FormatFloat(float64(tokens)/float64(unit.threshold), 'f', 3, 64)
			return strings.TrimRight(strings.TrimRight(value, "0"), ".") + unit.suffix
		}
	}
	return strconv.FormatInt(tokens, 10)
}

func periodTimestamp(milliseconds int64) string {
	if milliseconds <= 0 {
		return "unknown"
	}
	return time.UnixMilli(milliseconds).UTC().Format("2006-01-02 15:04:05")
}

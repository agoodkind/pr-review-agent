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
		cost := "cost unavailable"
		if period.CostComplete || period.PricedRequests > 0 {
			precision := 2
			if period.EstimatedCostUSD > 0 && period.EstimatedCostUSD < 0.01 {
				precision = int(-math.Floor(math.Log10(period.EstimatedCostUSD))) + 1
			}
			cost = "~$" + strconv.FormatFloat(period.EstimatedCostUSD, 'f', precision, 64)
		}
		parts := []string{
			label, compactPeriodTokens(period.TotalTokens) + " tokens / " + cost,
			periodTimestamp(period.Bounds.StartMS) + " to " + periodTimestamp(period.Bounds.EndMS),
			"Captured " + periodTimestamp(period.AsOfMS),
		}
		if !period.HistoryComplete {
			if period.Window.Mode == "" {
				parts = append(parts, "Recorded history begins "+periodTimestamp(period.HistoryStartMS))
			} else {
				parts = append(parts, "Partial history from "+periodTimestamp(period.HistoryStartMS))
			}
		}
		if !period.CostComplete && period.PricedRequests > 0 {
			parts = append(parts, fmt.Sprintf("Prices cover %d of %d requests", period.PricedRequests, period.ReportedRequests))
		}
		blocks = append(blocks, strings.Join(parts, "<br>"))
	}
	return strings.Join(blocks, "<br><br>")
}

func providerPeriodWindowLabel(window quota.Window) string {
	label := usageModelCell(string(window.Mode))
	if window.Mode == "" {
		return "Recorded usage"
	}
	if window.Duration != "" {
		label += " " + usageModelCell(window.Duration)
	}
	if window.Schedule != "" {
		label += " " + usageModelCell(window.Schedule) + " " + usageModelCell(window.Timezone)
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
	return time.UnixMilli(milliseconds).UTC().Format("2006-01-02 15:04:05 UTC")
}

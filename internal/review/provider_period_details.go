package review

import (
	"encoding/base64"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

const providerPeriodScope = "Period usage covers stored pr-review-agent reports across all models for each provider. Dollar amounts estimate cost from configured prices and do not represent account charges."

var providerPeriodKeyPattern = regexp.MustCompile(`<!-- pr-review-agent:period-usage:v1 provider=([A-Za-z0-9_-]*) model=([A-Za-z0-9_-]*) -->`)

func providerPeriodCell(key ProviderModelKey, periods []ProviderPeriodUsage) string {
	identity := fmt.Sprintf("<!-- pr-review-agent:period-usage:v1 provider=%s model=%s -->",
		base64.RawURLEncoding.EncodeToString([]byte(key.ProviderID)),
		base64.RawURLEncoding.EncodeToString([]byte(key.RequestedModel)))
	if len(periods) == 0 {
		return identity + "unavailable"
	}
	blocks := make([]string, 0, len(periods))
	for _, period := range periods {
		blocks = append(blocks, providerPeriodSnapshotCell(period))
	}
	return identity + strings.Join(blocks, "<br><br>")
}

func providerPeriodSnapshotCell(period ProviderPeriodUsage) string {
	label := providerPeriodWindowLabel(period.Window)
	if !period.Available {
		if period.Window.Mode == "" {
			return "token quota periods not configured"
		}
		return label + "<br>unavailable"
	}
	cost := "cost unavailable"
	if period.CostComplete || period.PricedRequests > 0 {
		cost = "~$" + compactPeriodCost(period.EstimatedCostUSD)
	}
	parts := []string{label, compactPeriodTokens(period.TotalTokens) + " tokens / " + cost}
	parts = append(parts, periodTimestamp(period.Bounds.StartMS)+" to "+periodTimestamp(period.Bounds.EndMS))
	parts = append(parts, "Captured "+periodTimestamp(period.AsOfMS))
	if !period.HistoryComplete {
		parts = append(parts, "Partial history from "+periodTimestamp(period.HistoryStartMS))
	}
	if !period.CostComplete && period.PricedRequests > 0 {
		parts = append(parts, fmt.Sprintf("Cost is incomplete: prices cover %d of %d requests.", period.PricedRequests, period.ReportedRequests))
	}
	return strings.Join(parts, "<br>")
}

func providerPeriodWindowLabel(window quota.Window) string {
	switch window.Mode {
	case quota.Rolling:
		return "Rolling " + usageModelCell(window.Duration)
	case quota.Fixed:
		return "Fixed " + usageModelCell(window.Duration)
	case quota.Cron:
		return "Cron " + usageModelCell(window.Schedule) + " " + usageModelCell(window.Timezone)
	}
	return "Quota period"
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
			return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(float64(tokens)/float64(unit.threshold), 'f', 3, 64), "0"), ".") + unit.suffix
		}
	}
	return strconv.FormatInt(tokens, 10)
}

func compactPeriodCost(cost float64) string {
	precision := 2
	if cost > 0 && cost < 0.01 {
		precision = int(-math.Floor(math.Log10(cost))) + 1
	}
	return strconv.FormatFloat(cost, 'f', precision, 64)
}

func periodTimestamp(milliseconds int64) string {
	if milliseconds <= 0 {
		return "unknown"
	}
	return time.UnixMilli(milliseconds).UTC().Format("2006-01-02 15:04:05.999 UTC")
}

func refreshProviderPeriodDetails(body string, usage UsageSummary) string {
	if !slices.Contains(usage.ProviderColumns, reviewrules.ProviderPeriodUsageColumn) {
		return body
	}
	start := strings.Index(body, "\n#### Provider details\n\n")
	if start < 0 {
		_, end, err := reviewDetailsRange(body, "preserved provider details")
		if err != nil {
			return body
		}
		closing := end - len("</details>")
		return body[:closing] + renderProviderDetails(usage) + "\n" + body[closing:]
	}
	lines := strings.Split(body[start:], "\n")
	if len(lines) < 5 || !strings.HasPrefix(lines[3], "|") || !strings.HasPrefix(lines[4], "|") {
		return body
	}
	header := splitProviderDetailRow(lines[3])
	periodIndex := slices.Index(header, "Period usage")
	if periodIndex < 0 {
		header = append(header, "Period usage")
		periodIndex = len(header) - 1
		lines[3] = joinProviderDetailRow(header)
		separator := splitProviderDetailRow(lines[4])
		lines[4] = joinProviderDetailRow(append(separator, "---"))
	}
	for index := 5; index < len(lines) && strings.HasPrefix(lines[index], "|"); index++ {
		cells := splitProviderDetailRow(lines[index])
		key, periods := preservedProviderPeriods(cells, header, usage.PeriodUsage)
		if periodIndex == len(cells) {
			cells = append(cells, "")
		}
		if periodIndex < len(cells) {
			cells[periodIndex] = providerPeriodCell(key, periods)
			lines[index] = joinProviderDetailRow(cells)
		}
	}
	updated := body[:start] + strings.Join(lines, "\n")
	if !strings.Contains(updated, providerPeriodScope) {
		updated = strings.Replace(updated, "\n#### Provider details\n\n", "\n"+providerPeriodScope+"\n\n#### Provider details\n\n", 1)
	}
	return updated
}

func preservedProviderPeriods(cells, header []string, periods []ProviderPeriodUsage) (ProviderModelKey, []ProviderPeriodUsage) {
	var key ProviderModelKey
	periodIndex := slices.Index(header, "Period usage")
	if periodIndex >= 0 && periodIndex < len(cells) {
		matches := providerPeriodKeyPattern.FindStringSubmatch(cells[periodIndex])
		if len(matches) == 3 {
			provider, providerErr := base64.RawURLEncoding.DecodeString(matches[1])
			model, modelErr := base64.RawURLEncoding.DecodeString(matches[2])
			if providerErr == nil && modelErr == nil {
				key.ProviderID, key.RequestedModel = string(provider), string(model)
			}
		}
	}
	providerIndex := slices.Index(header, "Provider")
	var matched []ProviderPeriodUsage
	for _, period := range periods {
		matches := key.ProviderID == period.ProviderID
		if key.ProviderID == "" && providerIndex >= 0 && providerIndex < len(cells) {
			matches = cells[providerIndex] == usageModelCell(period.ProviderID)
		}
		if matches {
			key.ProviderID = period.ProviderID
			if !slices.ContainsFunc(matched, func(snapshot ProviderPeriodUsage) bool { return snapshot.Window == period.Window }) {
				matched = append(matched, period)
			}
		}
	}
	return key, matched
}

func splitProviderDetailRow(line string) []string {
	var cells []string
	start := 1
	for index := 1; index < len(line); index++ {
		if line[index] == '|' && line[index-1] != '\\' {
			cells = append(cells, strings.TrimSpace(line[start:index]))
			start = index + 1
		}
	}
	return cells
}

func joinProviderDetailRow(cells []string) string {
	return "| " + strings.Join(cells, " | ") + " |"
}

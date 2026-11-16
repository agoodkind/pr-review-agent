package review

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

const rawNull = "null"

type rawDetailRow struct {
	Key   string
	Value string
}

type renderState struct {
	err error
}

func (state *renderState) capture(value string, err error) string {
	if state.err == nil && err != nil {
		state.err = err
	}

	return value
}

func (state *renderState) finish(value string) (string, error) {
	if state.err != nil {
		return "", state.err
	}

	return value, nil
}

func renderOutputParts(parts ...string) (string, error) {
	var rendering renderState

	resolved := make([]string, 0, len(parts))
	for _, part := range parts {
		resolved = append(resolved, rendering.capture(renderOutputText(part)))
	}

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.paragraphs", reflect.ValueOf(resolved))),
	)
}

func renderRawDetails(summary Summary) (string, error) {
	var rendering renderState
	usage := summary.Usage

	tiers := make([]string, 0, len(usage.ServiceTiers))
	for _, tier := range usage.ServiceTiers {
		tiers = append(tiers, rendering.capture(rawTableCell(tier)))
	}

	return rendering.finish(
		rendering.capture(
			renderOutputTemplate(
				"output.details",
				reflect.ValueOf(
					struct {
						Head                string
						Result              string
						Providers           string
						Usage               UsageSummary
						ServiceTiers        []string
						CacheWriteCostKnown bool
					}{
						Head: rendering.capture(
							rawTableCell(shortHead(summary.Head)),
						),
						Result:              rendering.capture(rawResultCell(summary.Result)),
						Providers:           rendering.capture(rawProviderUsage(usage)),
						Usage:               usage,
						ServiceTiers:        tiers,
						CacheWriteCostKnown: !hasUnpricedCacheWrites(usage),
					},
				),
			),
		),
	)
}

func rawResultCell(result string) (string, error) {
	var rendering renderState
	if result == "" {
		return rendering.finish(rendering.capture(rawTableCell(rawNull)))
	}

	if !strings.ContainsAny(result, "\r\n") {
		return rendering.finish(rendering.capture(rawTableCell(result)))
	}

	var escaped strings.Builder

	for _, character := range result {
		if strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~\r\n", character) {
			fmt.Fprintf(&escaped, "&#%d;", character)

			continue
		}

		escaped.WriteRune(character)
	}

	return rendering.finish(
		rendering.capture(
			renderOutputTemplate("output.multiline_result", reflect.ValueOf(escaped.String())),
		),
	)
}

func rawProviderUsage(usage UsageSummary) (string, error) {
	var rendering renderState
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

		cells = append(
			cells,
			rendering.capture(
				renderOutputTemplate(
					"output.provider_usage",
					reflect.ValueOf(rawDetailRow{
						Key:   rendering.capture(rawTableCell(provider)),
						Value: rendering.capture(rawTableCell(value)),
					}),
				),
			),
		)
	}

	if len(cells) == 0 {
		return rendering.finish(rendering.capture(rawTableCell(rawNull)))
	}

	return rendering.finish(
		rendering.capture(renderOutputTemplate("output.line_list", reflect.ValueOf(cells))),
	)
}

func hasUnpricedCacheWrites(usage UsageSummary) bool {
	for _, model := range usage.Models {
		if !model.Priced && model.CacheWriteTokens > 0 {
			return true
		}
	}

	return false
}

func usageModelCell(model string) (string, error) {
	var rendering renderState

	return rendering.finish(strings.ReplaceAll(rendering.capture(codeSpan(model)), "|", "&#124;"))
}

func rawTableCell(value string) (string, error) {
	var rendering renderState
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

	return rendering.finish(
		rendering.capture(
			renderOutputTemplate(
				"output.code_span",
				reflect.ValueOf(
					struct {
						Delimiter string
						Value     string
					}{
						Delimiter: delimiter,
						Value:     value,
					},
				),
			),
		),
	)
}

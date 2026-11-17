package review

import (
	"regexp"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

var legacyIncompleteLead = regexp.MustCompile(
	"^[0-9]+ chunks? could not be reviewed on `[0-9a-f]{7}`\\." +
		"(?: The app's daily token limit resets at 00:00 UTC\\.)?" +
		"(?: Apply the `" + regexp.QuoteMeta(domain.RerunReviewLabel) +
		"` label to retry this head\\.)?$",
)

const legacyFailureClass = "(?:the app token limit|a request timeout|an unexpected error|" +
	"an internal error|a provider service error|a provider rate limit|the provider usage limit)"

var legacyFailureDetail = regexp.MustCompile(
	"^(?:The review service encountered " + legacyFailureClass +
		"(?: \\(HTTP [0-9]{3}\\))? and could not complete the review\\.|" +
		"Provider `[^`]+` using model `[^`]+` encountered " + legacyFailureClass +
		"(?: \\(HTTP [0-9]{3}\\))? and could not complete the review\\.|" +
		"The cause is recorded in this service's log(?:, under run identifier `[^`]+`| for this run)\\.)$",
)

func stripLegacyFailureProse(body string) (string, error) {
	prefix, rest, found := strings.Cut(body, "\n\n")
	if !found || prefix != "## Review" {
		return body, nil
	}

	lead, remaining, found := strings.Cut(rest, "\n\n")
	if !found {
		return body, nil
	}

	lines, err := legacyFailureLines()
	if err != nil {
		return "", err
	}

	known := legacyFailureLine(lead, lines)
	if !known {
		return body, nil
	}

	for remaining != "" {
		paragraph, next, more := strings.Cut(remaining, "\n\n")
		if strings.HasPrefix(paragraph, "Resolve these inline findings:\n- ") {
			remaining = next

			continue
		}

		if strings.HasPrefix(
			remaining,
			"<details>\n<summary>Failure details</summary>\n\n"+
				"| Provider | Model | Failure | Status | Code |\n| --- | --- | --- | --- | --- |\n",
		) {
			end := strings.Index(remaining, "\n</details>")
			if end >= 0 {
				remaining = strings.TrimPrefix(remaining[end+len("\n</details>"):], "\n\n")

				continue
			}
		}

		known = legacyFailureLine(paragraph, lines)
		if !known {
			break
		}

		remaining = next
		if !more {
			break
		}
	}

	return strings.TrimSpace(prefix + "\n\n" + remaining), nil
}

func legacyFailureLine(paragraph string, lines map[string]struct{}) bool {
	if legacyIncompleteLead.MatchString(paragraph) || legacyFailureDetail.MatchString(paragraph) {
		return true
	}

	_, found := lines[paragraph]

	return found
}

func legacyFailureLines() (map[string]struct{}, error) {
	lines := make(map[string]struct{})

	for _, key := range []string{
		checkFailureAssessmentValidation,
		"output.check-failure-analysis",
		"output.check-failure-daily-budget",
		"output.check-failure-deadline",
		"output.check-failure-diff",
		"output.check-failure-mixed-rate-limit",
		"output.check-failure-mixed-usage",
		"output.check-failure-publish",
		"output.check-failure-pull-request",
		"output.check-failure-rate-limited",
		"output.check-failure-reconcile",
		"output.check-failure-refresh",
		"output.check-failure-reviews",
		"output.check-failure-skip",
		"output.check-failure-summary",
		"output.check-failure-threads",
		"output.check-failure-unavailable",
		"output.check-failure-usage",
		"output.check-summary-failure",
		"output.failure_panic",
		"output.unreviewed-head-reason",
	} {
		line, err := renderOutputText(key)
		if err != nil {
			return nil, err
		}

		lines[line] = struct{}{}
	}

	return lines, nil
}

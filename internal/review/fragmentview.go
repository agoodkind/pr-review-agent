package review

import (
	"reflect"
	"strings"
	"unicode/utf8"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reviewrules"
	"goodkind.io/pr-review-agent/internal/runlog"
)

func prepareUnreadSummary(summary Summary, shortfall structuralShortfall) (Summary, error) {
	reason, err := renderOutputTemplate("output.unread-verdict-reason", reflect.ValueOf(nil))
	if err != nil {
		return summary, err
	}

	summary.Decision = domain.ReviewDecisionComment
	summary.Omissions = shortfall.Hunks
	summary.Blocking = replaceBlockingReason(summary.Blocking, unreviewedHeadReason, reason)

	return summary, nil
}

func renderPendingScopeBody(
	summary Summary,
	notice string,
	policy reviewrules.Policy,
	statuses ...ProviderStatus,
) (string, error) {
	var data reviewrules.PromptData

	verdict, err := renderPrompt(policy, "coverage.verdict_pending", data)
	if err != nil {
		return "", err
	}

	return renderUnreadableBody(summary, notice, verdict, statuses...)
}

// maximumListedUnreadHunks bounds the list a notice prints.
//
// A check run output is capped by size, and one line per unread hunk over a
// pull request touching hundreds of files runs past that cap, which leaves the
// check unfinished and reports nothing at all. The count in the sentence above
// stays exact; only the list is cut.
const maximumListedUnreadHunks = 40

// maximumUnreadHunkLabelBytes bounds one line of that list, because a single
// repository path can be long enough to crowd out the rest on its own.
const maximumUnreadHunkLabelBytes = 220

// renderUnreadHunks lists each unread hunk as inert code text.
func renderUnreadHunks(hunks []unreadHunk) (string, error) {
	var state renderState

	listed := hunks

	omitted := 0
	if len(listed) > maximumListedUnreadHunks {
		omitted = len(listed) - maximumListedUnreadHunks
		listed = listed[:maximumListedUnreadHunks]
	}

	lines := make([]string, 0, len(listed))

	for _, hunk := range listed {
		lines = append(lines, state.capture(describeUnreadHunk(hunk)))
	}

	return state.finish(
		state.capture(
			renderOutputTemplate(
				"output.unread-hunks",
				reflect.ValueOf(
					struct {
						Lines   []string
						Omitted int
					}{
						lines,
						omitted,
					},
				),
			),
		),
	)
}

// describeUnreadHunk formats the source and reason for an unread fragment.
func describeUnreadHunk(hunk unreadHunk) (string, error) {
	var state renderState

	label := state.capture(
		renderOutputTemplate(
			"output.unread-hunk-label",
			reflect.ValueOf([]string{
				escapeUnreadHunkText(state.capture(hunk.sourceLabel())),
				escapeUnreadHunkText(hunk.Header),
			}),
		),
	)

	if len(label) > maximumUnreadHunkLabelBytes {
		label = state.capture(
			renderOutputTemplate(
				"output.truncated-label",
				reflect.ValueOf(truncateUTF8(label, maximumUnreadHunkLabelBytes)),
			),
		)
	}

	return state.finish(
		state.capture(
			renderOutputTemplate(
				"output.unread-hunk-reason",
				reflect.ValueOf([]string{label, state.capture(hunk.displayReason())}),
			),
		),
	)
}

// truncateUTF8 keeps the byte limit without splitting a rune.
func truncateUTF8(text string, maximumBytes int) string {
	end := min(len(text), maximumBytes)
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}

	return text[:end]
}

// escapeUnreadHunkText prevents repository text from closing the code fence.
func escapeUnreadHunkText(text string) string {
	withoutLines := runlog.EscapeLineBreaks(text)

	return strings.ReplaceAll(withoutLines, "`", "ˋ")
}

// RenderUnreadableBody renders a comment for an incomplete source read.
//
// The comment omits the review marker because the service did not review the
// complete head.
//
// For example:
//
//	body := RenderUnreadableBody(summary, notice, statuses...)
func RenderUnreadableBody(
	summary Summary,
	notice string,
	statuses ...ProviderStatus,
) (string, error) {
	var state renderState

	return state.finish(
		state.capture(
			renderUnreadableBody(
				summary,
				notice,
				state.capture(
					renderOutputTemplate("output.unreadable-verdict", reflect.ValueOf(nil)),
				),
				statuses...,
			),
		),
	)
}

func renderUnreadableBody(
	summary Summary,
	notice string,
	verdict string,
	statuses ...ProviderStatus,
) (string, error) {
	var state renderState

	return state.finish(
		state.capture(
			renderOutputTemplate(
				"output.unreadable-body",
				reflect.ValueOf(
					struct {
						VerdictStart string
						VerdictEnd   string
						Verdict      string
						Findings     string
						Report       string
						Notice       string
						Details      string
						Checkpoints  string
					}{
						verdictSectionStart,
						verdictSectionEnd,
						verdict,
						state.capture(renderSummaryFindings(summary)),
						state.capture(renderReportSection(summary.Report)),
						notice,
						state.capture(RenderDetails(summary, statuses...)),
						renderFindingCheckpoints(summary),
					},
				),
			),
		),
	)
}

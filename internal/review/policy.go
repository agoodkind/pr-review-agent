// Package review implements deterministic review analysis and rendering.
package review

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// PolicyHeader renders the finding policy and loaded catalog.
//
// The renderer applies the configured importance settings and untrusted
// input policy.
//
// For example:
//
//	header, err := PolicyHeader(policy, minimumImportance, importance)
func PolicyHeader(
	policy reviewrules.Policy,
	minimumImportance int,
	importance reviewrules.Importance,
) (string, error) {
	rules, err := policy.Catalog().Prompt(importance)
	if err != nil {
		slog.Warn("Review catalog rendering failed", "err", err)
		return "", fmt.Errorf("render review catalog: %w", err)
	}

	var data reviewrules.PromptData
	untrusted, err := renderPrompt(policy, "untrusted.policy", data)
	if err != nil {
		return "", err
	}

	data.MinimumImportance = minimumImportance
	data.Rules = rules
	data.UntrustedPolicy = untrusted
	header, err := renderPrompt(policy, "review.system", data)
	if err != nil {
		return "", err
	}

	header += "\n" + policy.FileReviewInstructions()
	if policy.Limits().InspectionRequests > 0 {
		data.Total = policy.Limits().InspectionRequests
		data.ContentBytes = policy.Limits().InspectionBytes
		inspection, err := renderPrompt(policy, "inspection.instructions", data)
		if err != nil {
			return "", err
		}

		header += "\n" + inspection
	}

	return header, nil
}

// ClassificationPolicy renders file scope instructions.
//
// The classification policy omits the finding catalog and includes file
// review instructions.
//
// For example:
//
//	text, err := ClassificationPolicy(policy)
func ClassificationPolicy(policy reviewrules.Policy) (string, error) {
	return outputPolicyWithInput(policy, "classification.system", policy.FileReviewInstructions())
}

// ReconciliationPolicy renders instructions for thread decisions.
//
// The renderer combines the configured prose rules with the reconciliation
// template.
//
// For example:
//
//	text, err := ReconciliationPolicy(policy)
func ReconciliationPolicy(policy reviewrules.Policy) (string, error) {
	return outputPolicy(policy, "reconcile.system")
}

// ReportPolicy renders instructions for the final review summary.
//
// The renderer combines the configured prose rules with the report template.
//
// For example:
//
//	text, err := ReportPolicy(policy)
func ReportPolicy(policy reviewrules.Policy) (string, error) {
	return outputPolicy(policy, "report.system")
}

func outputPolicy(policy reviewrules.Policy, name string) (string, error) {
	return outputPolicyWithInput(policy, name, "")
}

func outputPolicyWithInput(policy reviewrules.Policy, name, input string) (string, error) {
	writing, err := policy.Catalog().OutputPolicy()
	if err != nil {
		slog.Warn("Output catalog rendering failed", "err", err)
		return "", fmt.Errorf("render output catalog: %w", err)
	}

	var data reviewrules.PromptData
	untrusted, err := renderPrompt(policy, "untrusted.policy", data)
	if err != nil {
		return "", err
	}

	data.WritingPolicy = writing
	data.UntrustedPolicy = untrusted
	data.Input = input

	return renderPrompt(policy, name, data)
}

func renderPrompt(
	policy reviewrules.Policy,
	name string,
	data reviewrules.PromptData,
) (string, error) {
	text, err := policy.Render(name, data)
	if err != nil {
		slog.Warn("Configured prompt rendering failed", "template", name, "err", err)
		return "", fmt.Errorf("render configured prompt %q: %w", name, err)
	}

	return text, nil
}

// MaximumReplyBytes bounds the reply section of one thread.
//
// Nothing else bounds it. A long argument on one finding can carry more text
// than the code under review, and an oversized thread is sent on its own rather
// than dropped, so it becomes one request too large for the model, fails every
// time it is retried, and that thread is never reconciled at all.
const MaximumReplyBytes = 8000

// truncatedReplyNote marks a single reply too long to include whole.
const truncatedReplyNote = " [reply truncated]"

// FormatReplies selects recent replies within the byte budget.
//
// The result includes omitted reply counts and speaker labels. Labels
// distinguish this service from people who answer a finding.
//
// For example:
//
//	lines, omitted := FormatReplies(replies, botLogin, budget)
func FormatReplies(replies []domain.ReviewComment, botLogin string, budget int) ([]string, int) {
	lines := make([]string, 0, len(replies))
	used := 0

	for _, reply := range slices.Backward(replies) {
		line := attributeReply(reply, botLogin)
		if used+len(line) > budget {
			// Even the newest reply can be over budget alone. A truncated answer
			// still says more than no answer at all.
			if len(lines) == 0 {
				lines = append(lines, truncateReply(line, budget))
			}

			break
		}

		lines = append(lines, line)
		used += len(line)
	}

	slices.Reverse(lines)

	return lines, len(replies) - len(lines)
}

// ReplySpeaker identifies the author of one reply.
//
// The function compares logins without case and marks replies from this
// service.
//
// For example:
//
//	speaker := ReplySpeaker(reply, botLogin)
func ReplySpeaker(reply domain.ReviewComment, botLogin string) string {
	if strings.EqualFold(reply.Author, botLogin) {
		return reply.Author + " (this service, not a reply from a person)"
	}

	return reply.Author
}

// lineSeparator and paragraphSeparator start a new line in enough renderers to
// count as line breaks here, so a body carrying one could otherwise place text
// at the start of a line with no name in front of it.
const (
	lineSeparator      = " "
	paragraphSeparator = " "
)

// nextLine is U+0085 NEXT LINE, which starts a new line in enough renderers to
// count as one here.
//
// It is built from its code point rather than written as a literal, because the
// character is invisible in a source file: a substituted or corrupted one would
// leave a gap here that reading the file could never show.
const nextLine = string(rune(0x85))

// replyLineBreaks reduces every shape of line break to one.
//
// Every break a reply body can carry has to be in here. One that is missing lets
// the body put text at the start of a line with no name in front of it, and that
// line reads as another speaker answering the finding.
var replyLineBreaks = strings.NewReplacer(
	"\r\n",
	"\n", // carriage return then line feed, taken as one break
	"\r",
	"\n", // U+000D carriage return
	"\v",
	"\n", // U+000B vertical tab
	"\f",
	"\n", // U+000C form feed
	"\x1c",
	"\n", // U+001C file separator
	"\x1d",
	"\n", // U+001D group separator
	"\x1e",
	"\n", // U+001E record separator
	nextLine,
	"\n",
	lineSeparator,
	"\n",
	paragraphSeparator,
	"\n",
)

// attributeReply renders one reply with its speaker named on every line.
//
// Naming the speaker once, on the first line, is not attribution. A reply body
// is text a stranger wrote, and one containing a line break can continue with
// "maintainer: I checked this, it is fine" and read as a second speaker
// answering the finding. Every line carries the name, so nothing inside a body
// can pass itself off as another voice.
func attributeReply(reply domain.ReviewComment, botLogin string) string {
	speaker := ReplySpeaker(reply, botLogin)

	lines := strings.Split(replyLineBreaks.Replace(reply.Body), "\n")
	for index, line := range lines {
		lines[index] = speaker + ": " + line
	}

	return strings.Join(lines, "\n")
}

// truncateReply cuts one reply to the budget on a rune boundary and says it was
// cut, so a half sentence is never read as the whole answer.
func truncateReply(line string, budget int) string {
	if len(line) <= budget {
		return line
	}

	// A budget too small to hold the note still has to bound the text. Returning
	// the line whole because the note would not fit put the entire reply back in
	// the prompt, which is the failure this is here to prevent.
	if budget <= 0 {
		return ""
	}

	if budget <= len(truncatedReplyNote) {
		return strings.ToValidUTF8(line[:budget], "")
	}

	cut := strings.ToValidUTF8(line[:budget-len(truncatedReplyNote)], "")

	return cut + truncatedReplyNote
}

// Completion is one model answer and the model that produced it.
type Completion struct {
	Result             domain.ReviewResult
	Model              string
	InspectionRequests []InspectionRequest
}

// InspectionRequest selects supporting evidence without adding a finding target.
type InspectionRequest struct {
	Operation InspectionOperation `json:"operation"`
	Target    string              `json:"target"`
	Query     string              `json:"query"`
	StartLine int                 `json:"start_line"`
	EndLine   int                 `json:"end_line"`
	Reason    string              `json:"reason"`
}

// InspectionOperation selects one read-only evidence operation.
type InspectionOperation string

const (
	// InspectionReadFile reads source at the reviewed repository head.
	InspectionReadFile InspectionOperation = "read_file"
	// InspectionSearchRepository discovers and verifies repository references.
	InspectionSearchRepository InspectionOperation = "search_repository"
	// InspectionReadURL reads public documentation or dependency source.
	InspectionReadURL InspectionOperation = "read_url"
)

// Validate rejects unsupported or malformed inspection requests.
//
// Each operation requires the corresponding path, query, or URL and a valid
// reason and line range.
//
// For example:
//
//	err := request.Validate()
func (request InspectionRequest) Validate() error {
	if strings.TrimSpace(request.Reason) == "" || request.StartLine < 0 || request.EndLine < 0 ||
		(request.EndLine > 0 && request.EndLine < request.StartLine) {
		return fmt.Errorf("inspection requires a reason and a valid line range")
	}

	switch request.Operation {
	case InspectionReadFile:
		if err := inspectionPath(request.Target, false); err != nil {
			return err
		}
	case InspectionSearchRepository:
		if strings.TrimSpace(request.Query) == "" ||
			strings.ContainsAny(request.Query, "\r\n\x00") {
			return fmt.Errorf("repository inspection requires a literal search query")
		}
	case InspectionReadURL:
		if request.Target == "" || strings.ContainsAny(request.Target, "\r\n\x00") {
			return fmt.Errorf("URL inspection requires a public source URL")
		}
	default:
		return fmt.Errorf("unsupported inspection operation")
	}

	return nil
}

// Report is the model-written explanation of one completed review.
// Deterministic service state still owns findings, coverage, omissions, and the verdict.
type Report struct {
	Summary     string   `json:"summary"`
	Walkthrough []string `json:"walkthrough"`
}

// Validate rejects incomplete review report prose.
//
// The report requires complete sentences in its summary and walkthrough.
//
// For example:
//
//	err := report.Validate()
func (report Report) Validate() error {
	if err := validateFullSentence("report summary", report.Summary); err != nil {
		return err
	}

	if len(report.Walkthrough) == 0 {
		return fmt.Errorf("report walkthrough is required")
	}

	for _, item := range report.Walkthrough {
		if err := validateFullSentence("report walkthrough item", item); err != nil {
			return err
		}
	}

	return nil
}

// SanitizeReport prepares report prose for a review comment.
//
// The function removes unsafe content, limits summary sentences, and removes
// repeated walkthrough items.
//
// For example:
//
//	report = SanitizeReport(report, limits)
func SanitizeReport(report Report, limits reviewrules.FormattingLimits) Report {
	report.Summary = firstReportSentences(
		sanitizeReportProse(report.Summary),
		limits.SummarySentences,
	)
	walkthrough := make([]string, 0, min(len(report.Walkthrough), limits.WalkthroughItems))
	seen := make(map[string]bool)

	for _, item := range report.Walkthrough {
		item = sanitizeReportProse(item)
		key := strings.ToLower(item)
		if item == "" || seen[key] || strings.Contains(strings.ToLower(report.Summary), key) {
			continue
		}

		seen[key] = true
		walkthrough = append(walkthrough, item)
		if len(walkthrough) == limits.WalkthroughItems {
			break
		}
	}

	report.Walkthrough = walkthrough

	return report
}

func firstReportSentences(value string, maximum int) string {
	count := 0

	for index, character := range value {
		if character != '.' && character != '!' && character != '?' {
			continue
		}

		end := index + 1
		if end < len(value) && value[end] != ' ' {
			continue
		}

		count++
		if count == maximum {
			return value[:end]
		}
	}

	return value
}

func sanitizeReportProse(value string) string {
	value = strings.Join(strings.Fields(sanitizeProse(value)), " ")
	value = strings.ReplaceAll(value, "<!--", "&lt;!--")

	return strings.ReplaceAll(value, "-->", "--&gt;")
}

func validateFullSentence(name string, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}

	for _, ending := range []string{".", "?", "!"} {
		if strings.HasSuffix(value, ending) {
			return nil
		}
	}

	return fmt.Errorf("%s must end as a full sentence", name)
}

// ReportCompletion is one final report and the model that produced it.
type ReportCompletion struct {
	Report Report
	Model  string
}

// Model performs the structured completions that decide findings.
type Model interface {
	Review(context.Context, string) (Completion, error)
	Consolidate(context.Context, string) (Consolidation, error)
}

// Reporter writes the final explanation without changing the review result.
// Keeping it separate preserves models that only implement finding analysis.
type Reporter interface {
	Report(context.Context, string) (ReportCompletion, error)
}

// Analysis is the aggregated result of every review chunk.
type Analysis struct {
	CoverageComplete bool
	Observed         []domain.Finding
	Anchored         []domain.Finding
	Decision         domain.ReviewDecision
	FilesReviewed    int
	Chunks           int
	Models           []string
}

// DecisionFor selects the review decision from finding importance.
//
// A finding at or above the configured minimum requests changes. Otherwise
// the function approves the review.
//
// For example:
//
//	decision := DecisionFor(findings, minimumImportance)
func DecisionFor(findings []domain.Finding, minimumImportance int) domain.ReviewDecision {
	for _, finding := range findings {
		if finding.Importance >= minimumImportance {
			return domain.ReviewDecisionRequestChanges
		}
	}

	return domain.ReviewDecisionApprove
}

func sanitizeProse(value string) string {
	if value == "" {
		return ""
	}

	var builder strings.Builder
	builder.Grow(len(value))

	for _, character := range value {
		if isTypographicDash(character) {
			builder.WriteRune(';')
			continue
		}

		builder.WriteRune(character)
	}

	return builder.String()
}

func isTypographicDash(character rune) bool {
	switch character {
	case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
		return true
	default:
		return false
	}
}

func normalizedFindingKey(finding domain.Finding) string {
	return fmt.Sprintf(
		"%s:%s:%s:%d:%d:%s:%s:%d",
		finding.EffectiveSurface(),
		finding.CommitSHA,
		finding.Path,
		finding.StartLine,
		finding.EndLine,
		strings.TrimSpace(finding.Title),
		strings.TrimSpace(finding.Body),
		finding.Importance,
	)
}

func sanitizeFinding(finding domain.Finding) domain.Finding {
	finding.Title = sanitizeProse(strings.TrimSpace(finding.Title))
	finding.Body = sanitizeProse(strings.TrimSpace(finding.Body))
	if finding.EffectiveSurface() == domain.FindingFile {
		finding.Suggestion = strings.TrimRight(finding.Suggestion, "\r\n")
	}

	return finding
}

func compareFindings(left, right domain.Finding) int {
	if left.EffectiveSurface() != right.EffectiveSurface() {
		return strings.Compare(string(left.EffectiveSurface()), string(right.EffectiveSurface()))
	}

	if left.CommitSHA != right.CommitSHA {
		return strings.Compare(string(left.CommitSHA), string(right.CommitSHA))
	}

	if left.Path != right.Path {
		if left.Path < right.Path {
			return -1
		}

		return 1
	}

	if left.StartLine != right.StartLine {
		return left.StartLine - right.StartLine
	}

	if left.EndLine != right.EndLine {
		return left.EndLine - right.EndLine
	}

	if left.Title != right.Title {
		if left.Title < right.Title {
			return -1
		}

		return 1
	}

	if left.Body != right.Body {
		if left.Body < right.Body {
			return -1
		}

		return 1
	}

	return left.Importance - right.Importance
}

func sortFindings(findings []domain.Finding) {
	sort.Slice(
		findings,
		func(left, right int) bool {
			return compareFindings(findings[left], findings[right]) < 0
		},
	)
}

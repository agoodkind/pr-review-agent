package review

// This file keeps a claim the pull request has already answered from being
// raised again.
//
// One live pull request received the same produce authorization ask five times
// across five pushes. Each time the author replied on the thread with the
// evidence that disproved it, and each time the next run opened a brand new
// thread that referenced neither. The final run also asked for a config read
// its own previous round had asked to remove.
//
// Identity suppression cannot catch that. A finding's identity is a hash of its
// path and its normalized title, so a reworded title is a different finding, and
// the claim moved between two files as well. Nothing about those five is equal
// to anything, which is why the answer here is semantic rather than an equality
// test on a stronger key.
//
// It costs no extra model call. The open threads are already loaded for
// reconciliation, so the same data becomes context in the chunk prompt: what is
// open, and what the author said back. The model is told not to raise an
// answered claim again. A model that disobeys is not a defense, so a
// deterministic backstop drops a finding that restates an open thread's claim on
// the same file.

import (
	"context"
	"log/slog"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// logWithheldFindings reports every claim this chunk raised again and the run
// withheld, in one line. Without it a finding that never reaches the page is
// indistinguishable from one the model never made.
func logWithheldFindings(ctx context.Context, withheld []withheldFinding) {
	if len(withheld) == 0 {
		return
	}
	gklog.L(ctx).InfoContext(
		ctx,
		"findings withheld, already answered on an open thread",
		slog.Int("withheld", len(withheld)),
		slog.Any("findings", withheld),
	)
}

const (
	// maximumDisputeBytes bounds the open thread context added to one chunk
	// prompt. The chunk itself is already the larger half of the budget, and a
	// pull request with many open threads would otherwise push the code the run
	// is supposed to be reviewing out of the model's input. Threads that do not
	// fit are still covered by the deterministic backstop, which reads them all.
	maximumDisputeBytes = 16000
	// minimumDisputeEvidenceLength is how much evidence has to be there before
	// it can suppress anything. A one or two token line such as a closing brace
	// appears in almost any prose, so matching on it would drop unrelated real
	// findings. A line long enough to identify the code it came from does not.
	minimumDisputeEvidenceLength = 16
	// disputeReasonTitle and disputeReasonEvidence name which arm of the
	// backstop fired, so the log says why a finding was withheld.
	disputeReasonTitle    = "same title on an open thread"
	disputeReasonEvidence = "same evidence line on an open thread"
)

// disputeContext is what the pull request has already been told, in the two
// forms the run needs it: prose for the model, and keys for the backstop.
//
// Only unresolved threads are in it. A resolved thread is a settled question,
// and a defect reintroduced after a fix deserves a new finding rather than
// silence, so resolving must never suppress.
type disputeContext struct {
	// sections is one block per open thread, already truncated to the budget.
	sections []string
	// openIDs are the finding identities the open threads carry, which is path
	// and normalized title together.
	openIDs map[string]struct{}
	// openBodies maps a normalized path to the rendered bodies of the open
	// threads anchored in that file.
	openBodies map[string][]string
}

// collectDisputes reads the open findings of the service's own from the threads
// the run already loaded for reconciliation.
func collectDisputes(threads []githubapp.ReviewThread, botLogin string) disputeContext {
	disputes := disputeContext{
		sections:   make([]string, 0),
		openIDs:    make(map[string]struct{}),
		openBodies: make(map[string][]string),
	}
	budget := maximumDisputeBytes
	for _, thread := range threads {
		if thread.Resolved || thread.RootComment.Author != botLogin {
			continue
		}
		_, finding, err := marker.DecodeFindingBody(thread.RootComment)
		if err != nil {
			continue
		}
		normalizedPath, err := marker.NormalizePath(finding.Path)
		if err != nil {
			continue
		}
		if findingID, idErr := marker.FindingID(finding); idErr == nil {
			disputes.openIDs[findingID] = struct{}{}
		}
		disputes.openBodies[normalizedPath] = append(
			disputes.openBodies[normalizedPath],
			finding.Title+"\n"+finding.Body,
		)

		section := formatDisputeSection(normalizedPath, finding, thread.Replies)
		if len(section) <= budget {
			disputes.sections = append(disputes.sections, section)
			budget -= len(section)
		}
	}
	return disputes
}

// formatDisputeSection renders one open thread as the model sees it: where the
// claim was made, what it said, and what the author answered.
func formatDisputeSection(
	normalizedPath string,
	finding domain.Finding,
	replies []domain.ReviewComment,
) string {
	var builder strings.Builder
	builder.WriteString("Open finding\nPath: ")
	builder.WriteString(normalizedPath)
	builder.WriteString("\nTitle: ")
	builder.WriteString(finding.Title)
	builder.WriteString("\nBody: ")
	builder.WriteString(finding.Body)
	if len(replies) == 0 {
		builder.WriteString("\nAnswered: no reply yet.")
		return builder.String()
	}
	builder.WriteString("\nAnswered by the pull request author:")
	for _, reply := range replies {
		builder.WriteString("\n")
		builder.WriteString(reply.Author)
		builder.WriteString(": ")
		builder.WriteString(reply.Body)
	}
	return builder.String()
}

// promptSection is the open thread context for one chunk prompt, or an empty
// string when nothing is open.
//
// The instruction sits outside the untrusted delimiters because it is this
// service speaking. The threads and the replies sit inside them, because a
// reply is text a stranger wrote on a public pull request.
func (disputes disputeContext) promptSection() string {
	if len(disputes.sections) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(
		"These findings are already open on this pull request, with the author's answers where there are any. " +
			"A claim already raised and answered here must not be raised again in any wording, under any title, at any path. " +
			"If an author's reply is factually wrong, quote the reply and say why it is wrong; do not restate the original claim as though it were unanswered.\n",
	)
	builder.WriteString(WrapUntrusted(strings.Join(disputes.sections, "\n\n")))
	builder.WriteString("\n")
	return builder.String()
}

// withheldFinding is one finding the pull request has already answered, kept
// with the reason so the run can report the whole set in one line.
type withheldFinding struct {
	Path   string `json:"path"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// partition splits findings into the ones still worth raising and the ones this
// pull request has already answered.
func (disputes disputeContext) partition(
	findings []domain.Finding,
) ([]domain.Finding, []withheldFinding) {
	unanswered := make([]domain.Finding, 0, len(findings))
	withheld := make([]withheldFinding, 0)
	for _, finding := range findings {
		reason, answered := disputes.answered(finding)
		if !answered {
			unanswered = append(unanswered, finding)
			continue
		}
		withheld = append(withheld, withheldFinding{
			Path:   finding.Path,
			Title:  finding.Title,
			Reason: reason,
		})
	}
	return unanswered, withheld
}

// answered reports whether an open thread already carries this claim, and which
// arm of the test says so.
//
// It is deliberately narrow. Both arms require the same file, because a claim
// about one file says nothing about the same words in another, and neither arm
// looks at a resolved thread.
func (disputes disputeContext) answered(finding domain.Finding) (string, bool) {
	normalizedPath, err := marker.NormalizePath(finding.Path)
	if err != nil {
		return "", false
	}
	if findingID, idErr := marker.FindingID(finding); idErr == nil {
		if _, open := disputes.openIDs[findingID]; open {
			return disputeReasonTitle, true
		}
	}

	evidence := strings.TrimSpace(finding.Evidence)
	if len(evidence) < minimumDisputeEvidenceLength {
		return "", false
	}
	for _, body := range disputes.openBodies[normalizedPath] {
		if strings.Contains(body, evidence) {
			return disputeReasonEvidence, true
		}
	}
	return "", false
}

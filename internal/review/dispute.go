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
// to anything.
//
// So the mechanism here is the prompt, and it costs no extra model call. The
// open threads are already loaded for reconciliation, so the same data becomes
// context in the chunk prompt: what is open, and what anyone replied. The model
// is told not to raise an answered claim again.
//
// Beside it runs a deterministic backstop, because a model told not to repeat
// itself is not a guarantee that it will not.
//
// The backstop compares claim keys. A claim key is the finding's path and its
// evidence line hashed together, carried in the published marker, so it survives
// every rewording and answers the question the title cannot: is this the same
// claim about the same code. Two earlier attempts are why it is this and not
// something looser. Matching an evidence line against an open thread's prose
// withheld a genuinely separate defect on the same file. Matching the finding
// identity instead was measured to be a subset of the suppression
// collectPublicationState already applies, so it decided nothing at all.
//
// Only open threads suppress. A resolved thread is a settled question, and a
// defect reintroduced after a fix has to be raised again. A marker written
// before the claim key existed carries none, and suppresses nothing.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// withheldFinding is one claim the run did not raise again, with the thread that
// already carries it.
type withheldFinding struct {
	Path   string `json:"path"`
	Title  string `json:"title"`
	Thread string `json:"thread"`
}

// logWithheldFindings names every claim this chunk raised again and the run
// withheld, and the thread each one matched.
//
// Withholding is otherwise invisible. The finding never reaches the page, so
// without this line a suppression that is working and a model that found nothing
// look exactly alike, and a suppression that is too broad looks like both.
func logWithheldFindings(ctx context.Context, withheld []withheldFinding) {
	if len(withheld) == 0 {
		return
	}
	gklog.L(ctx).InfoContext(
		ctx,
		"findings withheld, already open on this pull request",
		slog.Int("withheld", len(withheld)),
		slog.Any("findings", withheld),
	)
}

// maximumDisputeBytes bounds the open thread context added to one chunk prompt.
// The chunk itself is already the larger half of the budget, and a pull request
// with many open threads would otherwise push the code the run is supposed to be
// reviewing out of the model's input.
const maximumDisputeBytes = 16000

// disputeContext is what the pull request has already been told, rendered for
// the chunk prompt.
//
// Only unresolved threads are in it. A resolved thread is a settled question,
// and telling the model about it would argue against raising a defect that has
// since come back.
type disputeContext struct {
	// sections is one block per open thread, already truncated to the budget.
	sections []string
	// openClaims maps the claim key of each open thread to the thread that
	// carries it, so a withheld finding can name what answered it. A thread
	// whose marker has no claim key is not in here and suppresses nothing.
	openClaims map[string]string
}

// answered reports the open thread already carrying this finding's claim.
//
// The claim key covers the path, so a match is always a claim about the same
// file. A finding with no evidence derives no key and is never withheld, which
// is moot in practice because the grounding gate refuses it first.
func (disputes disputeContext) answered(finding domain.Finding) (string, bool) {
	claim, err := marker.ClaimKey(finding.Path, finding.Evidence)
	if err != nil {
		return "", false
	}
	thread, open := disputes.openClaims[claim]
	return thread, open
}

// collectDisputes reads the open findings of the service's own from the threads
// the run already loaded for reconciliation.
func collectDisputes(threads []githubapp.ReviewThread, botLogin string) disputeContext {
	disputes := disputeContext{
		sections:   make([]string, 0),
		openClaims: make(map[string]string),
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
		// The empty check is belt and braces: ClaimKey refuses to derive a key
		// from a finding with no evidence, so an empty key can never be looked
		// up either. Indexing one anyway would make this depend on that.
		if published, ok := marker.FindFinding(thread.RootComment.Body); ok && published.ClaimKey != "" {
			disputes.openClaims[published.ClaimKey] = thread.NodeID
		}

		section := formatDisputeSection(normalizedPath, finding, thread.Replies, botLogin)
		if len(section) <= budget {
			disputes.sections = append(disputes.sections, section)
			budget -= len(section)
		}
	}
	return disputes
}

// formatDisputeSection renders one open thread as the model sees it: where the
// claim was made, what it said, and what anyone has replied.
//
// The replies are not labelled as the author's. Anyone who can comment on a pull
// request can reply on a thread, so presenting every reply as the author's
// answer would let a passer by, or this service quoting itself, stand as the
// authority that withholds a valid finding. Each line names its speaker and the
// model is left to weigh it.
func formatDisputeSection(
	normalizedPath string,
	finding domain.Finding,
	replies []domain.ReviewComment,
	botLogin string,
) string {
	var builder strings.Builder
	builder.WriteString("Open finding\nPath: ")
	builder.WriteString(normalizedPath)
	builder.WriteString("\nTitle: ")
	builder.WriteString(finding.Title)
	builder.WriteString("\nBody: ")
	builder.WriteString(finding.Body)
	if len(replies) == 0 {
		builder.WriteString("\nReplies: none yet.")
		return builder.String()
	}
	lines, omitted := FormatReplies(replies, botLogin, MaximumReplyBytes)
	builder.WriteString("\nReplies, oldest first. The name before each one is who wrote it")
	if omitted > 0 {
		fmt.Fprintf(&builder, ", and %d older replies are not shown", omitted)
	}
	builder.WriteString(":")
	for _, line := range lines {
		builder.WriteString("\n")
		builder.WriteString(line)
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
		"These findings are already open on this pull request, with any replies they have received. " +
			"A claim already raised and answered here must not be raised again in any wording, under any title, at any path. " +
			"Weigh a reply by who wrote it and whether the code bears it out. " +
			"If a reply is factually wrong, quote it and say why it is wrong; do not restate the original claim as though it were unanswered.\n",
	)
	builder.WriteString(WrapUntrusted(strings.Join(disputes.sections, "\n\n")))
	builder.WriteString("\n")
	return builder.String()
}

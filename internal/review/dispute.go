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
// There is deliberately no deterministic backstop beside it. One was built and
// removed: matching a new finding's evidence line against an open thread's prose
// withheld valid findings, and the strict identity that replaced it was measured
// to be a subset of the suppression collectPublicationState already applies, so
// it decided nothing. A backstop that works needs a durable claim key in the
// published finding marker, because a published comment carries no evidence to
// compare against. That is a change to the marker format, not to this file.

import (
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

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
}

// collectDisputes reads the open findings of the service's own from the threads
// the run already loaded for reconciliation.
func collectDisputes(threads []githubapp.ReviewThread, botLogin string) disputeContext {
	disputes := disputeContext{sections: make([]string, 0)}
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
	builder.WriteString("\nReplies, oldest first. The name before each one is who wrote it:")
	for _, reply := range replies {
		builder.WriteString("\n")
		builder.WriteString(replySpeaker(reply, botLogin))
		builder.WriteString(": ")
		builder.WriteString(reply.Body)
	}
	return builder.String()
}

// replySpeaker names who wrote one reply, marking this service's own replies so
// its own words never read back to it as somebody else's answer.
func replySpeaker(reply domain.ReviewComment, botLogin string) string {
	if reply.Author == botLogin {
		return reply.Author + " (this service, not a reply from a person)"
	}
	return reply.Author
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

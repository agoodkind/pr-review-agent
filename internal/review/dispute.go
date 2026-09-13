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
// The backstop compares claim keys and anchor ranges, through the one comparison
// every layer of duplicate suppression shares. A claim key is the finding's path
// and its evidence line hashed together, carried in the published marker, so it
// survives every rewording and answers the question the title cannot: is this
// the same claim about the same code. An anchor range catches the restatement
// that rests on a neighbouring line, and it is all a comment published before the
// claim key existed has. Two earlier attempts are why the comparison is these and
// not something looser. Matching an evidence line against an open thread's prose
// withheld a genuinely separate defect on the same file. Matching the finding
// identity instead was measured to be a subset of the suppression
// collectPublicationState already applies, so it decided nothing at all.
//
// Open threads suppress across heads. A resolved thread suppresses only at the
// head where the service raised it, so a forced rereview remembers what this
// reviewer already settled without hiding a defect reintroduced by new code.

import (
	"fmt"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// maximumDisputeBytes bounds the reviewer context added to one chunk prompt.
// The chunk itself is already the larger half of the budget, and a pull request
// with many open threads would otherwise push the code the run is supposed to be
// reviewing out of the model's input.
const maximumDisputeBytes = 16000

// disputeContext is what the pull request has already been told, rendered for
// the chunk prompt.
//
// Open threads are always in it. Resolved threads from the current head stay in
// it because a forced rereview must retain what this reviewer already settled.
type disputeContext struct {
	// sections is one block per relevant thread, already truncated to the budget.
	sections []string
	// known is what every relevant thread of the service's own already claims,
	// keyed by claim key and by anchor range and labelled with the thread that
	// carries it, so a withheld finding can name what answered it.
	//
	// A thread whose marker has no claim key still contributes its anchor range,
	// which is what every comment published before the key existed has. Both are
	// compared through the same function the within-run layers use.
	known *claimMemory
}

// answered reports the thread already carrying this finding's claim.
//
// Both comparisons cover the path, so a match is always a claim about the same
// file. A finding that derives neither key is never withheld, which is moot in
// practice because the grounding and anchoring gates refuse it first.
func (disputes disputeContext) answered(finding domain.Finding) (duplicateMatch, bool) {
	return disputes.known.match(candidateKeys(finding))
}

// collectDisputes reads every current inline discussion from the threads the
// run already loaded. The service's own findings also populate the deterministic
// duplicate backstop.
func collectDisputes(
	threads []githubapp.ReviewThread,
	botLogin string,
	currentHead domain.HeadSHA,
) disputeContext {
	disputes := disputeContext{
		sections: make([]string, 0),
		known:    newClaimMemory(),
	}
	budget := maximumDisputeBytes
	// Open discussions retain first claim on the bounded prompt. Resolved
	// discussions use only the space left after every active conversation.
	for _, resolved := range []bool{false, true} {
		for _, thread := range threads {
			if thread.Resolved != resolved {
				continue
			}
			normalizedPath, err := marker.NormalizePath(thread.RootComment.Path)
			if err != nil {
				continue
			}
			section := formatDiscussionSection(thread, normalizedPath, botLogin)
			if owned, ok := ownedDisputeSection(
				thread, normalizedPath, botLogin, currentHead, disputes.known,
			); ok {
				section = owned
			}
			if len(section) <= budget {
				disputes.sections = append(disputes.sections, section)
				budget -= len(section)
			}
		}
	}
	return disputes
}

func ownedDisputeSection(
	thread githubapp.ReviewThread,
	normalizedPath string,
	botLogin string,
	currentHead domain.HeadSHA,
	known *claimMemory,
) (string, bool) {
	if thread.RootComment.Author != botLogin {
		return "", false
	}
	published, ok := marker.FindFinding(thread.RootComment.Body)
	if !ok {
		return "", false
	}
	_, finding, err := marker.DecodeFindingBody(thread.RootComment)
	if err != nil {
		return "", false
	}
	current := published.Head == currentHead
	if !thread.Resolved || current {
		known.remember(threadKeys(published, thread.RootComment), thread.NodeID)
	}
	return formatDisputeSection(
		normalizedPath,
		finding,
		thread.Replies,
		botLogin,
		thread.Resolved,
		current,
	), true
}

func formatDiscussionSection(
	thread githubapp.ReviewThread,
	normalizedPath string,
	botLogin string,
) string {
	state := "Open inline discussion"
	if thread.Resolved {
		state = "Resolved inline discussion"
	}
	var builder strings.Builder
	builder.WriteString(state)
	builder.WriteString("\nPath: ")
	builder.WriteString(normalizedPath)
	builder.WriteString("\nRoot comment by ")
	builder.WriteString(ReplySpeaker(thread.RootComment, botLogin))
	builder.WriteString(": ")
	builder.WriteString(thread.RootComment.Body)
	if len(thread.Replies) == 0 {
		builder.WriteString("\nReplies: none yet.")
		return builder.String()
	}
	lines, omitted := FormatReplies(thread.Replies, botLogin, MaximumReplyBytes)
	builder.WriteString("\nReplies, oldest first")
	if omitted > 0 {
		fmt.Fprintf(&builder, ", with %d older replies not shown", omitted)
	}
	builder.WriteString(":")
	for _, line := range lines {
		builder.WriteString("\n")
		builder.WriteString(line)
	}
	return builder.String()
}

// formatDisputeSection renders one relevant thread as the model sees it: where
// the claim was made, what it said, and what anyone has replied.
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
	resolved bool,
	currentHead bool,
) string {
	var builder strings.Builder
	switch {
	case resolved && currentHead:
		builder.WriteString("Resolved finding from this commit\nPath: ")
	case resolved:
		builder.WriteString("Resolved finding from an earlier commit\nPath: ")
	default:
		builder.WriteString("Open finding\nPath: ")
	}
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

// promptSection is the relevant thread context for one chunk prompt, or an
// empty string when the reviewer has raised nothing relevant.
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
		"These are the current inline discussions and replies on the pull request. " +
			"A resolved finding from this exact commit is settled and must not be raised again. " +
			"A resolved finding from an earlier commit is context to reconsider against the current code and replies. It does not decide the current review by itself. " +
			"Before raising a concern already discussed, decide whether the current code and replies support it. Do not repeat it when the discussion correctly answers it. " +
			"Weigh each reply by who wrote it and whether the current code bears it out. " +
			"If a reply is factually wrong, quote it and say why it is wrong; do not restate the original claim as though it were unanswered.\n",
	)
	builder.WriteString(WrapUntrusted(strings.Join(disputes.sections, "\n\n")))
	builder.WriteString("\n")
	return builder.String()
}

package review

// This file asks the model, once per chunk, whether the findings that chunk
// still stands behind are really several findings.
//
// The deterministic layers compare a claim key, a claim sentence, and an anchor
// range. Every one of them is exact, which is what makes them safe to run with
// no model call, and also what leaves a gap: two restatements that cite
// different lines of one function, under different titles, with the model
// having written a different claim sentence for each, share none of the three.
// pr-review-agent 89 published four comments for one defect that way.
//
// Reading is the only thing that closes that gap, so one extra call reads them.
// It sees the chunk's remaining candidates and the threads already open with
// their replies, and it answers with groups: candidates that state one defect,
// and groups that state what an open thread already states.
//
// The call is bounded hard. One per chunk that still holds two or more
// candidates, never one per candidate, and never a retry. A chunk with one
// candidate has nothing to group and pays nothing. An answer that fails or does
// not parse publishes what the deterministic layers left and says so in the
// log, because a reviewer that loses findings to its own tidying is worse than
// one that repeats itself.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
)

// minimumConsolidationCandidates is how many candidates a chunk must hold to be
// worth asking about on their own. One candidate can restate nothing in its own
// chunk; it can still restate a thread already open, which is what the open
// thread half of the gate is for.
const minimumConsolidationCandidates = 2

// maximumConsolidationReasonBytes bounds the model's own sentence in a log line.
// The reason is there to explain one grouping to a reader, and an answer that
// writes an essay must not push the rest of the line out of view.
const maximumConsolidationReasonBytes = 200

// senseConsolidation names the layer that read the candidates rather than
// comparing keys.
const senseConsolidation = "consolidation"

// ConsolidationGroup is one set of a chunk's candidates the model says state a
// single defect.
type ConsolidationGroup struct {
	// Candidates are the numbers the prompt showed, counting from one.
	Candidates []int `json:"candidates"`
	// RestatesOpenThread marks a group that states what a finding already open
	// on this pull request states. Every member of such a group is dropped,
	// because the thread is where that conversation already is.
	RestatesOpenThread bool `json:"restates_open_thread"`
	// Reason is the model's one line for why these are one defect.
	Reason string `json:"reason"`
}

// Consolidation is the model's grouping of one chunk's candidates.
type Consolidation struct {
	Groups []ConsolidationGroup `json:"groups"`
}

// ValidateShape rejects a grouping that is malformed however many candidates it
// was asked about: a group naming nothing, a number below one, or a candidate
// placed in two groups.
//
// The model client applies this where it decodes the answer, the way it
// validates a review result and a set of thread resolutions, so a malformed
// answer is refused while the provider that produced it is still in view.
//
// It cannot be the whole check. The upper bound on a candidate number is the
// number of candidates the caller showed, and the client holds only a prompt
// string, so it does not know that number. Validate adds it.
func (consolidation Consolidation) ValidateShape() error {
	claimed := make(map[int]struct{})
	for _, group := range consolidation.Groups {
		if len(group.Candidates) == 0 {
			return errors.New("consolidation group names no candidate")
		}
		for _, number := range group.Candidates {
			if number < 1 {
				return fmt.Errorf("consolidation names candidate %d, below the first", number)
			}
			if _, repeated := claimed[number]; repeated {
				return fmt.Errorf("consolidation places candidate %d in two groups", number)
			}
			claimed[number] = struct{}{}
		}
	}
	return nil
}

// Validate rejects a grouping that could not be applied to the candidates it
// was asked about.
//
// A number outside the range, or one placed in two groups, means the answer is
// not about the findings that were shown. Applying it anyway would drop
// findings on the strength of a grouping nobody can check, so the whole answer
// is refused and the deterministic result publishes instead.
//
// This runs on every answer, whichever client produced it, and it is what stands
// between a malformed grouping and the merge. The boundary check in the client
// is the same test applied earlier and against less: it cannot know how many
// candidates were shown.
func (consolidation Consolidation) Validate(candidateCount int) error {
	if err := consolidation.ValidateShape(); err != nil {
		return err
	}
	for _, group := range consolidation.Groups {
		for _, number := range group.Candidates {
			if number > candidateCount {
				return fmt.Errorf("consolidation names candidate %d of %d", number, candidateCount)
			}
		}
	}
	return nil
}

// ConsolidationPolicy is the instruction for grouping one chunk's candidates.
func ConsolidationPolicy() string {
	return "Group findings that state one defect, however differently each is worded and wherever each is anchored. " +
		"Two findings are one defect when fixing one fixes the other. Leave findings that need separate fixes ungrouped. " +
		"Never invent a finding number and never place one number in two groups.\nWriting policy: " +
		config.WritingPolicy + "\nUntrusted input policy: " + UntrustedInputPolicy
}

// chunkPosts turns one chunk's answer into the comments to post.
//
// The consolidation call sits between two locked stages and holds no lock
// itself, because a model call under the pass lock would stop every other chunk
// from posting while it ran.
func (service *Service) chunkPosts(
	ctx context.Context,
	head domain.HeadSHA,
	chunkText string,
	findings []domain.Finding,
	pass *chunkPass,
) []postCandidate {
	candidates := pass.chunkCandidates(ctx, chunkText, findings)
	return service.renderChunkFindings(ctx, head, service.consolidateChunk(ctx, candidates, pass), pass)
}

// chunkCandidates is what one chunk answer still stands behind after the
// deterministic layers: grounded in the source shown, anchored to changed
// lines, above the importance floor, carried by nobody else, and collapsed to
// one candidate per claim.
func (pass *chunkPass) chunkCandidates(
	ctx context.Context,
	chunkText string,
	findings []domain.Finding,
) []domain.Finding {
	pass.mu.Lock()
	defer pass.mu.Unlock()

	pass.collector.collect(findings)
	grounded := groundedFindings(ctx, findings, pass.collector.fileIndex, chunkText)
	eligible := eligibleFindings(grounded, pass.collector.fileIndex, pass.collector.minimumImportance)
	return collapseChunkCandidates(ctx, unansweredCandidates(ctx, eligible, pass))
}

// worthConsolidating reports whether a chunk's surviving candidates are worth
// one extra model call.
//
// Two things can be asked about, and either one is enough. Two candidates can
// restate each other. One candidate can restate a thread already open, and
// only when there is such a thread to show it: the deterministic layers have
// already compared it against every open thread by claim key, claim text and
// anchor, so what is left is a restatement that shares none of the three, and
// nothing but reading the two can see that.
//
// A lone candidate was free until a probe published one. An open thread quoted
// one line, the next run reported the same defect at another line in other
// words under another title, the chunk held that finding alone, and it reached
// the page. That is the failure across pushes this file exists to close, and it
// is not a corner: agoodkind/tack 169 took exactly that shape five times.
//
// A chunk with no candidates is always free, which is most chunks of most
// deltas, and a pull request carrying no open finding of this service's own
// pays nothing new either.
func worthConsolidating(candidates []domain.Finding, disputes string) bool {
	if len(candidates) == 0 {
		return false
	}
	if len(candidates) >= minimumConsolidationCandidates {
		return true
	}
	// The prompt section is the test rather than the thread count, because it is
	// what the model will actually be shown. A thread the byte budget dropped is
	// a thread the call could not weigh anything against.
	return disputes != ""
}

// consolidateChunk asks the model once whether the candidates this chunk still
// holds state one defect between them, or state what an open thread states.
//
// It returns the candidates unchanged whenever there is nothing to ask about,
// whenever the call fails, and whenever the answer cannot be applied. Losing a
// real finding to a failed tidying pass is the one outcome worse than the
// repeats this layer exists to remove.
func (service *Service) consolidateChunk(
	ctx context.Context,
	candidates []domain.Finding,
	pass *chunkPass,
) []domain.Finding {
	if !worthConsolidating(candidates, pass.disputePrompt) {
		return candidates
	}

	// The call gets its own clock built from the caller's, exactly as a chunk
	// call does, so it inherits no budget another call has already spent.
	callCtx, cancel := context.WithTimeout(ctx, service.chunkTimeout)
	answer, err := service.model.Consolidate(
		callCtx,
		buildConsolidationPrompt(candidates, pass.disputePrompt),
	)
	cancel()
	pass.recordConsolidationRequest()
	if err == nil {
		err = answer.Validate(len(candidates))
	}
	if err != nil {
		gklog.L(ctx).WarnContext(
			ctx,
			"consolidation call failed, publishing what the deterministic layers left",
			slog.Int("candidates", len(candidates)),
			slog.String("err", err.Error()),
		)
		return candidates
	}
	return applyConsolidation(ctx, candidates, answer)
}

// applyConsolidation keeps one candidate per group and drops the rest.
//
// A group that restates an open thread loses every member: the thread already
// carries that conversation, and reopening it beside the old one is the failure
// across pushes that the whole of this file exists to stop. Any other group
// keeps its strongest member, on the same rule the deterministic collapse uses.
func applyConsolidation(
	ctx context.Context,
	candidates []domain.Finding,
	answer Consolidation,
) []domain.Finding {
	dropped := make(map[int]duplicateMatch, len(candidates))
	for _, group := range answer.Groups {
		survivor := strongestCandidate(group.Candidates, candidates)
		for _, number := range group.Candidates {
			if number == survivor && !group.RestatesOpenThread {
				continue
			}
			dropped[number] = consolidationMatch(group, candidates[survivor-1])
		}
	}

	kept := make([]domain.Finding, 0, len(candidates))
	for index, candidate := range candidates {
		match, isDropped := dropped[index+1]
		if !isDropped {
			kept = append(kept, candidate)
			continue
		}
		logSuppressed(ctx, layerConsolidation, candidate, match)
	}
	return kept
}

// consolidationMatch names what one dropped candidate was found to repeat.
func consolidationMatch(group ConsolidationGroup, survivor domain.Finding) duplicateMatch {
	matched := survivor.Title
	if group.RestatesOpenThread {
		matched = "a finding already open on this pull request"
	}
	return duplicateMatch{
		Sense:   senseConsolidation,
		Detail:  truncateReply(strings.TrimSpace(group.Reason), maximumConsolidationReasonBytes),
		Matched: matched,
	}
}

// strongestCandidate returns the number of the group member to keep: the
// highest importance, and the lowest number among equals.
//
// The tie is broken on the number rather than on where the number sits in the
// group, so the same group returns the same survivor whichever order the model
// listed it in. Seeding from the first number and replacing only on strictly
// higher importance read as though it did that, and did not: a group returned
// as [2, 1] with two equal candidates kept 2, which made the survivor a
// property of the answer's phrasing rather than of the findings. Deciding by
// the phrasing is the whole failure this file exists to remove.
//
// The number counts from one and indexes the candidate list in the order the
// chunk produced it, so the lowest number is the earliest finding, which is the
// tie the deterministic collapse breaks on too.
func strongestCandidate(numbers []int, candidates []domain.Finding) int {
	strongest := numbers[0]
	for _, number := range numbers {
		importance := candidates[number-1].Importance
		strongestImportance := candidates[strongest-1].Importance
		if importance > strongestImportance {
			strongest = number
			continue
		}
		if importance == strongestImportance && number < strongest {
			strongest = number
		}
	}
	return strongest
}

// buildConsolidationPrompt asks for the grouping of one chunk's candidates.
//
// The open threads come first, for the same reason they come first in a chunk
// prompt: what has already been raised has to be in view before the model
// decides what these findings add to it. The instruction is outside the
// untrusted delimiters because this service wrote it; the findings and the
// threads sit inside them, because both are model output and stranger prose.
func buildConsolidationPrompt(candidates []domain.Finding, disputes string) string {
	var builder strings.Builder
	builder.WriteString(disputes)
	builder.WriteString("These numbered findings all came from one review chunk. ")
	builder.WriteString("Return a group for every set of numbers that state one defect between them. ")
	builder.WriteString(
		"Set restates_open_thread on a group that states what a finding already open on this pull request states. ",
	)
	builder.WriteString("A finding that repeats nothing belongs in no group.\n")
	builder.WriteString(WrapUntrusted(formatConsolidationCandidates(candidates)))
	return builder.String()
}

// formatConsolidationCandidates renders the candidates as the model sees them,
// numbered from one.
func formatConsolidationCandidates(candidates []domain.Finding) string {
	sections := make([]string, 0, len(candidates))
	for index, candidate := range candidates {
		sections = append(sections, fmt.Sprintf(
			"Finding %d\nPath: %s\nLines: %d-%d\nTitle: %s\nClaim: %s\nBody: %s\nEvidence: %s",
			index+1,
			candidate.Path,
			candidate.StartLine,
			candidate.EndLine,
			candidate.Title,
			candidate.Claim,
			candidate.Body,
			candidate.Evidence,
		))
	}
	return strings.Join(sections, "\n\n")
}

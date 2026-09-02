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

// minimumConsolidationCandidates is how many candidates a chunk must still hold
// for the call to be worth making. One candidate can restate nothing.
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

// Validate rejects a grouping that could not be applied to the candidates it
// was asked about.
//
// A number outside the range, or one placed in two groups, means the answer is
// not about the findings that were shown. Applying it anyway would drop
// findings on the strength of a grouping nobody can check, so the whole answer
// is refused and the deterministic result publishes instead.
func (consolidation Consolidation) Validate(candidateCount int) error {
	claimed := make(map[int]struct{}, candidateCount)
	for _, group := range consolidation.Groups {
		if len(group.Candidates) == 0 {
			return errors.New("consolidation group names no candidate")
		}
		for _, number := range group.Candidates {
			if number < 1 || number > candidateCount {
				return fmt.Errorf("consolidation names candidate %d of %d", number, candidateCount)
			}
			if _, repeated := claimed[number]; repeated {
				return fmt.Errorf("consolidation places candidate %d in two groups", number)
			}
			claimed[number] = struct{}{}
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
	if len(candidates) < minimumConsolidationCandidates {
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
// highest importance, and the earliest number among equals.
func strongestCandidate(numbers []int, candidates []domain.Finding) int {
	strongest := numbers[0]
	for _, number := range numbers {
		if candidates[number-1].Importance > candidates[strongest-1].Importance {
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

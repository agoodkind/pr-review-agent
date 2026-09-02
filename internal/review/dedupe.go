package review

// This file holds the one comparison that decides whether two findings are the
// same claim, and the memory the suppression layers consult.
//
// Several layers suppress repeats, and every one of them asks the same
// question: is this candidate a restatement of something already carried. One
// layer asks it of a chunk's own answer, one of what an earlier chunk in this
// run published, one of what an open thread on this pull request already says.
// They ask it here, because three copies of one question drift apart, and a
// repeat then reaches the page through whichever copy has fallen behind.
//
// The comparison is deliberately narrow. Matching an evidence line against an
// open thread's prose withheld a genuinely separate defect on the same file, so
// nothing here compares prose. It compares three things: the claim key, which is
// the path and the evidence line hashed together; the claim text key, which is
// the model's own canonical label for the defect hashed on its own; and the
// anchor range, which is the lines the finding objects to. Two findings sharing
// any of the three are about the same defect.
//
// The claim sentence is the one of the three that survives a restatement citing
// a different line of the same function, which is why layer 0 asks the model for
// it. The two hashes are compared, never the sentences: a hash matches exactly
// and shows nothing.
//
// One live run is why this exists. pr-review-agent 89 at head 98f509a published
// seven threads in 57 seconds carrying three distinct claims: one claim appeared
// four times under four titles, another twice, and one was genuine.

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

// The layer a suppression log line names, so a reader can tell which layer
// withheld a finding and go straight to it.
const (
	layerWithinChunk   = "within one chunk"
	layerAcrossChunks  = "across chunks in this run"
	layerOpenThreads   = "against open threads"
	layerPriorReviews  = "against earlier reviews"
	layerConsolidation = "consolidation call"
)

// The senses in which one finding can repeat another.
const (
	senseClaimKey    = "claim key"
	senseClaimText   = "claim text"
	senseAnchorRange = "anchor range"
	senseIdentity    = "finding identity"
	senseAnchorLine  = "anchor line"
)

// claimKeyLogLength is how much of a 64 character key one log line prints.
// Twelve characters match two lines against each other by eye and leave the
// rest of the line readable.
const claimKeyLogLength = 12

// duplicateKeys is everything the comparison reads off one finding, whether the
// finding is a candidate this run produced or a thread an earlier run left open.
//
// Every key is optional and independent. A finding with no evidence derives no
// claim key, one from an answer that carried no claim sentence derives no claim
// text key, and one whose path or range the marker refuses derives no anchor.
// A value here can carry any of them, all of them, or none, and one that
// carries none matches nothing.
type duplicateKeys struct {
	claimKey     string
	claimTextKey string
	path         string
	startLine    int
	endLine      int
	rangeValid   bool
}

// keysFrom builds the comparison keys from the two claim keys and an anchor.
func keysFrom(
	claimKey string,
	claimTextKey string,
	pathValue string,
	startLine int,
	endLine int,
) duplicateKeys {
	keys := duplicateKeys{
		claimKey:     claimKey,
		claimTextKey: claimTextKey,
		path:         "",
		startLine:    0,
		endLine:      0,
		rangeValid:   false,
	}
	normalizedPath, err := marker.NormalizePath(pathValue)
	if err != nil || startLine < 1 || endLine < startLine {
		return keys
	}
	keys.path = normalizedPath
	keys.startLine = startLine
	keys.endLine = endLine
	keys.rangeValid = true
	return keys
}

// candidateKeys reads the comparison keys off one finding this run produced.
func candidateKeys(finding domain.Finding) duplicateKeys {
	claimKey := ""
	if value, err := marker.ClaimKey(finding.Path, finding.Evidence); err == nil {
		claimKey = value
	}
	claimTextKey := ""
	if value, err := marker.ClaimTextKey(finding.Claim); err == nil {
		claimTextKey = value
	}
	return keysFrom(claimKey, claimTextKey, finding.Path, finding.StartLine, finding.EndLine)
}

// threadKeys reads the comparison keys off one thread an earlier run opened.
//
// Both claim keys come from the published marker rather than from the decoded
// finding. A published comment prints neither the evidence line nor the claim
// sentence, only their hashes, so a finding decoded back out of one could derive
// no key at all.
func threadKeys(published marker.FindingMarker, comment domain.ReviewComment) duplicateKeys {
	return keysFrom(
		published.ClaimKey,
		published.ClaimTextKey,
		comment.Path,
		comment.StartLine,
		comment.EndLine,
	)
}

// duplicateMatch names why one finding repeats another and what it repeats.
type duplicateMatch struct {
	// Sense is which comparison fired.
	Sense string
	// Detail is that comparison's evidence: the shared key, or the two ranges.
	Detail string
	// Matched names the thing already carried, so a reader can go read it.
	Matched string
}

// noMatch is the answer when nothing already carried matches.
func noMatch() duplicateMatch {
	return duplicateMatch{Sense: "", Detail: "", Matched: ""}
}

// sameClaim reports whether a candidate repeats something already carried, and
// in which sense.
//
// The two arguments are not interchangeable in the report: the detail reads as
// the candidate measured against the carried finding, which is the direction a
// reader of the log line is thinking in.
func sameClaim(carried duplicateKeys, candidate duplicateKeys) (duplicateMatch, bool) {
	if carried.claimKey != "" && carried.claimKey == candidate.claimKey {
		return duplicateMatch{
			Sense:   senseClaimKey,
			Detail:  shortClaimKey(carried.claimKey),
			Matched: "",
		}, true
	}
	if carried.claimTextKey != "" && carried.claimTextKey == candidate.claimTextKey {
		return duplicateMatch{
			Sense:   senseClaimText,
			Detail:  shortClaimKey(carried.claimTextKey),
			Matched: "",
		}, true
	}
	if rangesOverlap(carried, candidate) {
		return duplicateMatch{
			Sense:   senseAnchorRange,
			Detail:  describeOverlap(carried, candidate),
			Matched: "",
		}, true
	}
	return noMatch(), false
}

// rangesOverlap reports whether two findings object to overlapping lines of one
// file.
//
// Overlap rather than equality is the test because the anchor history compared
// exact end lines, and a restatement anchored one line over passed it. Two
// findings whose ranges share a line are objecting to the same code.
func rangesOverlap(left duplicateKeys, right duplicateKeys) bool {
	if !left.rangeValid || !right.rangeValid {
		return false
	}
	if left.path != right.path {
		return false
	}
	return left.startLine <= right.endLine && right.startLine <= left.endLine
}

// describeOverlap states the candidate's range and the range it overlaps.
func describeOverlap(carried duplicateKeys, candidate duplicateKeys) string {
	return fmt.Sprintf(
		"%s:%d-%d over %s:%d-%d",
		candidate.path,
		candidate.startLine,
		candidate.endLine,
		carried.path,
		carried.startLine,
		carried.endLine,
	)
}

// shortClaimKey trims a claim key to the prefix a log line prints.
func shortClaimKey(claimKey string) string {
	if len(claimKey) <= claimKeyLogLength {
		return claimKey
	}
	return claimKey[:claimKeyLogLength]
}

// claimMemory is what has already been carried, and the name each thing goes by
// in a log line.
//
// One layer fills it from the findings this run has already published, so a
// later chunk cannot repost what an earlier chunk carried. Another fills it from
// the threads still open on the pull request, so this run cannot repost what an
// earlier run carried. Both read it through sameClaim, so the two cannot drift.
//
// It is a scan rather than a map because the comparison is not a lookup: a range
// overlap has no key to hash. The sets are the findings of one run and the open
// threads of one pull request, both tens of entries, so the scan costs nothing
// worth a second index.
type claimMemory struct {
	keys   []duplicateKeys
	labels []string
}

func newClaimMemory() *claimMemory {
	return &claimMemory{keys: make([]duplicateKeys, 0), labels: make([]string, 0)}
}

// remember records one thing as carried, under the name a log line will use.
func (memory *claimMemory) remember(keys duplicateKeys, label string) {
	memory.keys = append(memory.keys, keys)
	memory.labels = append(memory.labels, label)
}

// match reports the first carried thing this candidate repeats.
func (memory *claimMemory) match(candidate duplicateKeys) (duplicateMatch, bool) {
	slot, found, ok := firstMatch(memory.keys, candidate)
	if !ok {
		return noMatch(), false
	}
	found.Matched = memory.labels[slot]
	return found, true
}

// firstMatch returns the position of the first carried key the candidate
// repeats, and why.
func firstMatch(carried []duplicateKeys, candidate duplicateKeys) (int, duplicateMatch, bool) {
	for slot, keys := range carried {
		if found, same := sameClaim(keys, candidate); same {
			return slot, found, true
		}
	}
	return 0, noMatch(), false
}

// collapseChunkCandidates collapses one chunk answer's own candidates, so a
// chunk that reported one defect three ways posts it once.
//
// The survivor of a group is the member with the highest importance, and the
// earliest in the model's order among equals. A restatement that rates the same
// defect higher is still the better comment to publish, and the model's own
// order breaks the tie because nothing else about two restatements differs.
//
// Grouping is greedy against the survivors so far rather than transitive, which
// keeps every suppression a direct comparison between the finding withheld and
// the finding kept. That is what the log line names, and a reader checking one
// has both sides in front of them.
func collapseChunkCandidates(ctx context.Context, candidates []domain.Finding) []domain.Finding {
	survivors := make([]domain.Finding, 0, len(candidates))
	survivorKeys := make([]duplicateKeys, 0, len(candidates))
	for _, candidate := range candidates {
		keys := candidateKeys(candidate)
		slot, found, same := firstMatch(survivorKeys, keys)
		if !same {
			survivors = append(survivors, candidate)
			survivorKeys = append(survivorKeys, keys)
			continue
		}
		if candidate.Importance > survivors[slot].Importance {
			found.Matched = candidate.Title
			logSuppressed(ctx, layerWithinChunk, survivors[slot], found)
			survivors[slot] = candidate
			survivorKeys[slot] = keys
			continue
		}
		found.Matched = survivors[slot].Title
		logSuppressed(ctx, layerWithinChunk, candidate, found)
	}
	return survivors
}

// logSuppressed records one finding a layer withheld: which layer, on what
// evidence, and what it matched.
//
// Withholding is otherwise invisible. The finding never reaches the page, so
// without this line a suppression that is working and a model that found nothing
// look exactly alike, and a suppression that is too broad looks like both.
func logSuppressed(
	ctx context.Context,
	layer string,
	finding domain.Finding,
	match duplicateMatch,
) {
	gklog.L(ctx).InfoContext(
		ctx,
		"finding suppressed as a repeat",
		slog.String("layer", layer),
		slog.String("sense", match.Sense),
		slog.String("detail", match.Detail),
		slog.String("matched", match.Matched),
		slog.String("path", finding.Path),
		slog.String("title", finding.Title),
		slog.Int("importance", finding.Importance),
	)
}

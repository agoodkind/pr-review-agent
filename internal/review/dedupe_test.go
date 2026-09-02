package review_test

// These tests drive the whole service and read what reached the pull request,
// because that is the only place duplicate suppression is observable. A run
// that finds three restatements of one defect and a reader who sees three
// comments is the failure, and the count of inline comments is what says so.
//
// The measured case is pr-review-agent 89 at head 98f509a: seven threads in 57
// seconds carrying three distinct claims, one of them under four titles.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/review"
)

// noConsolidation answers every consolidation call with no groups.
//
// That is the honest neutral answer: a model that groups nothing is saying
// these findings are several findings. A double that grouped everything would
// make every publication count in this package pass for the wrong reason, and
// one that returned an error would put a fallback in every log line.
type noConsolidation struct{}

func (noConsolidation) Consolidate(context.Context, string) (review.Consolidation, error) {
	return review.Consolidation{Groups: nil}, nil
}

// oneChunkFixture wires a run whose single chunk answers with findings against
// the three changed lines stubCollector adds.
func oneChunkFixture(t *testing.T, findings ...domain.Finding) *serviceFixture {
	t.Helper()
	return newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{{
			Findings: findings,
		}}},
	})
}

// publishedBodies runs the fixture and returns the inline comments that reached
// the pull request.
func publishedBodies(t *testing.T, fixture *serviceFixture) []string {
	t.Helper()
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return bodiesOf(fixture.state.streamedComments)
}

// One answer quoting one source line twice is one claim, whatever it calls
// itself the second time. Nothing about the two titles matches and the two
// anchors are two lines apart, so identity and the anchor line both pass them.
//
// The survivor is the higher rated of the two. A restatement that rates the
// same defect higher is the better comment to publish, and the reader gets one
// comment rather than a pair that disagree about how much it matters.
func TestOneChunkPublishesOneCommentForOneClaimQuotedTwice(t *testing.T) {
	bodies := publishedBodies(t, oneChunkFixture(
		t,
		domain.Finding{
			Path:       "main.go",
			StartLine:  2,
			EndLine:    2,
			Title:      "Lower rated wording",
			Body:       "The changed line breaks core behavior.",
			Evidence:   "added",
			Suggestion: "",
			Importance: 9,
		},
		domain.Finding{
			Path:       "main.go",
			StartLine:  4,
			EndLine:    4,
			Title:      "Higher rated wording",
			Body:       "The same defect, stated again and rated higher.",
			Evidence:   "+added",
			Suggestion: "",
			Importance: 10,
		},
	))

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: both rest on the same source line", bodies)
	}
	if !strings.Contains(bodies[0], "Higher rated wording") {
		t.Fatalf("published comment = %q, want the higher rated of the two restatements", bodies[0])
	}
}

// Two findings objecting to overlapping lines of one file are two boxes in the
// same place. They quote different source lines, so no claim key joins them and
// only the overlap can.
func TestOneChunkPublishesOneCommentForOverlappingAnchors(t *testing.T) {
	bodies := publishedBodies(t, oneChunkFixture(
		t,
		domain.Finding{
			Path:       "main.go",
			StartLine:  2,
			EndLine:    3,
			Title:      "Spanning defect",
			Body:       "The changed lines break core behavior.",
			Evidence:   "added",
			Suggestion: "",
			Importance: 9,
		},
		domain.Finding{
			Path:       "main.go",
			StartLine:  3,
			EndLine:    4,
			Title:      "Overlapping restatement",
			Body:       "The same region, stated again over a shifted range.",
			Evidence:   "third",
			Suggestion: "",
			Importance: 10,
		},
	))

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: the two ranges share line 3", bodies)
	}
	if !strings.Contains(bodies[0], "Overlapping restatement") {
		t.Fatalf("published comment = %q, want the higher rated of the two", bodies[0])
	}
}

// The suppression line exists so that a wrong suppression can be seen, so it
// has to describe the finding that was withheld rather than the one kept.
//
// The case that gets this wrong is a higher rated restatement arriving second
// and taking the survivor's slot, because the finding withheld is then the one
// already held rather than the one that just arrived. The overlap detail states
// two ranges in order, withheld first, so reusing the comparison computed for
// the arriving candidate prints the pair backwards.
func TestTheSuppressionLogNamesTheWithheldFindingsRangeFirst(t *testing.T) {
	logs := &syncBuffer{}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		logWriter:         logs,
		model: &sequenceModel{results: []domain.ReviewResult{{
			Findings: []domain.Finding{
				{
					Path:       "main.go",
					StartLine:  2,
					EndLine:    3,
					Title:      "Spanning defect",
					Body:       "The changed lines break core behavior.",
					Evidence:   "added",
					Claim:      "The spanning range is wrong",
					Suggestion: "",
					Importance: 9,
				},
				{
					Path:       "main.go",
					StartLine:  3,
					EndLine:    4,
					Title:      "Overlapping restatement",
					Body:       "The same region, stated again over a shifted range.",
					Evidence:   "third",
					Claim:      "The shifted range is wrong",
					Suggestion: "",
					Importance: 10,
				},
			},
		}}},
	})

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: the two ranges share line 3", bodies)
	}
	const withheldFirst = "main.go:2-3 over main.go:3-4"
	const keptFirst = "main.go:3-4 over main.go:2-3"
	if strings.Contains(logs.String(), keptFirst) {
		t.Fatalf("suppression detail read %q, want the withheld finding's range first, not the kept one's", keptFirst)
	}
	if !strings.Contains(logs.String(), withheldFirst) {
		t.Fatalf("service log = %q, want a suppression detail reading %q", logs.String(), withheldFirst)
	}
}

// Collapsing restatements must not collapse a review. Two findings resting on
// two source lines, anchored apart, are two defects and both belong on the page.
func TestTwoDistinctDefectsInOneChunkAnswerBothPublish(t *testing.T) {
	bodies := publishedBodies(t, oneChunkFixture(
		t,
		domain.Finding{
			Path:       "main.go",
			StartLine:  2,
			EndLine:    2,
			Title:      "First defect",
			Body:       "The changed line breaks core behavior.",
			Evidence:   "added",
			Suggestion: "",
			Importance: 9,
		},
		domain.Finding{
			Path:       "main.go",
			StartLine:  4,
			EndLine:    4,
			Title:      "Second defect",
			Body:       "A separate defect resting on a separate line.",
			Evidence:   "third",
			Suggestion: "",
			Importance: 9,
		},
	))

	if len(bodies) != 2 {
		t.Fatalf("published comments = %v, want both: different lines, different claims", bodies)
	}
}

// sharedClaimLine is the one source line both chunks' findings rest on in the
// cross-chunk test. Both files carry it, so a finding about the first file
// grounds whichever chunk reported it.
const sharedClaimLine = "if err := publish(ctx); err != nil {"

// twoFileChunkCollector returns two files each large enough to be its own
// chunk, both carrying sharedClaimLine among their changed lines.
//
// The content agrees with the patch: the hunk says line 1 is context and lines
// 2 through 4 are added, so the content opens with those four lines in that
// order and the padding follows. It used to open with the padding, which put
// the quoted lines thirty thousand lines below where the patch said they were.
// No pull request can produce that, and a grounding or anchoring test passing
// on it is a test passing on an artifact.
type twoFileChunkCollector struct{}

func (twoFileChunkCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	padding := strings.Repeat("x\n", 30000)
	files := make([]diff.FileContext, 0, 2)
	for index := range 2 {
		patch := fmt.Sprintf(
			"@@ -1,1 +1,4 @@\n line%d\n+%s\n+other%d\n+third%d\n",
			index,
			sharedClaimLine,
			index,
			index,
		)
		changed, hunks, err := diff.ChangedRightLines(patch)
		if err != nil {
			return diff.ReviewInput{}, err
		}
		files = append(files, diff.FileContext{
			Path:   fmt.Sprintf("file%d.go", index),
			Status: "modified",
			Patch:  patch,
			CurrentContent: fmt.Sprintf(
				"line%d\n%s\nother%d\nthird%d\n%s",
				index,
				sharedClaimLine,
				index,
				index,
				padding,
			),
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		})
	}
	return diff.ReviewInput{PullRequest: pullRequest, Files: files}, nil
}

// crossChunkRestatement is one chunk's answer: the shared claim, worded its own
// way and anchored on its own line of the first file.
func crossChunkRestatement(title string, line int) domain.ReviewResult {
	return domain.ReviewResult{
		Findings: []domain.Finding{{
			Path:       "file0.go",
			StartLine:  line,
			EndLine:    line,
			Title:      title,
			Body:       "The publish failure is ignored.",
			Evidence:   sharedClaimLine,
			Suggestion: "",
			Importance: 9,
		}},
	}
}

// Chunks are reviewed concurrently and no chunk sees another's answer, so the
// only thing that stops two chunks reporting one defect twice is what the run
// remembers between them. Identity did not stop it: the two titles differ, so
// the two hashes differ. The claim key survives the rewording.
func TestTwoChunksReportingOneClaimPublishItOnce(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         twoFileChunkCollector{},
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{
			crossChunkRestatement("Publish failure is ignored", 2),
			crossChunkRestatement("Publish result is discarded", 3),
		}},
	})

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: both chunks rest on the same source line", bodies)
	}
}

// sharedClaimSentence is the canonical label two restatements of one defect
// carry. The two arrive capitalized and punctuated differently, which is what a
// model writing the same label twice actually produces.
const sharedClaimSentence = "The publish error is ignored"

// claimTextRestatement is one wording of the shared claim: its own title, its
// own line, its own quoted source line, and the one label in common.
func claimTextRestatement(title string, line int, evidence string, importance int) domain.Finding {
	return domain.Finding{
		Path:       "main.go",
		StartLine:  line,
		EndLine:    line,
		Title:      title,
		Body:       "The failure of this call is not handled.",
		Evidence:   evidence,
		Claim:      sharedClaimSentence,
		Suggestion: "",
		Importance: importance,
	}
}

// Two restatements of one defect share no title, no anchor, and no quoted
// source line, so identity, the anchor range, and the claim key all pass them.
// The claim sentence is what they have in common, and it is the only thing that
// recognizes this shape. One live pull request received the same ask five times
// this way.
//
// The survivor is the higher rated of the two.
func TestOneChunkPublishesOneCommentForOneClaimStatedTwice(t *testing.T) {
	bodies := publishedBodies(t, oneChunkFixture(
		t,
		claimTextRestatement("Publish failure is ignored", 2, "added", 9),
		claimTextRestatement("publish error goes unchecked.", 4, "third", 10),
	))

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: both name the same defect", bodies)
	}
	if !strings.Contains(bodies[0], "publish error goes unchecked") {
		t.Fatalf("published comment = %q, want the higher rated of the two restatements", bodies[0])
	}
}

// crossChunkClaimText is one chunk's answer: the shared claim sentence about
// the first file, worded its own way and resting on its own quoted line.
func crossChunkClaimText(title string, line int, evidence string) domain.ReviewResult {
	return domain.ReviewResult{
		Findings: []domain.Finding{{
			Path:       "file0.go",
			StartLine:  line,
			EndLine:    line,
			Title:      title,
			Body:       "The failure of this call is not handled.",
			Evidence:   evidence,
			Claim:      sharedClaimSentence,
			Suggestion: "",
			Importance: 9,
		}},
	}
}

// No chunk sees another chunk's answer, so two chunks naming one defect in one
// file is the cross-run failure happening inside a single run. The two quote
// different lines and anchor two lines apart, so the claim key and the overlap
// both pass them and only the claim sentence joins them.
func TestTwoChunksNamingOneDefectPublishItOnce(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         twoFileChunkCollector{},
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{
			crossChunkClaimText("Publish failure is ignored", 2, sharedClaimLine),
			crossChunkClaimText("Publish error goes unchecked", 4, "third0"),
		}},
	})

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: both chunks name the same defect in one file", bodies)
	}
}

// One claim sentence is a short label for a defect, and a label is true of many
// files at once. Two findings carrying the identical label about two unrelated
// files are two defects, and suppressing either loses a review.
func TestOneClaimSentenceInTwoFilesPublishesTwice(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         twoFileChunkCollector{},
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{
			{
				Findings: []domain.Finding{{
					Path:       "file0.go",
					StartLine:  2,
					EndLine:    2,
					Title:      "Unhandled error in the first file",
					Body:       "The failure of this call is not handled.",
					Evidence:   sharedClaimLine,
					Claim:      sharedClaimSentence,
					Suggestion: "",
					Importance: 9,
				}},
			},
			{
				Findings: []domain.Finding{{
					Path:       "file1.go",
					StartLine:  2,
					EndLine:    2,
					Title:      "Unhandled error in the second file",
					Body:       "The failure of this call is not handled.",
					Evidence:   sharedClaimLine,
					Claim:      sharedClaimSentence,
					Suggestion: "",
					Importance: 9,
				}},
			},
		}},
	})

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 2 {
		t.Fatalf("published comments = %v, want both: one label about two files is two defects", bodies)
	}
}

// claimThreadFinding is the claim already standing on the pull request, carrying
// the claim sentence its marker hashes into claimtext.
func claimThreadFinding() domain.Finding {
	return domain.Finding{
		Path:       "main.go",
		StartLine:  2,
		EndLine:    2,
		Title:      "Standing claim",
		Body:       "The failure of this call is not handled.",
		Evidence:   "added",
		Claim:      sharedClaimSentence,
		Suggestion: "",
		Importance: 9,
	}
}

// claimThreadFixture wires a run whose one chunk names the defect a thread
// already carries, from another line and under another title.
func claimThreadFixture(t *testing.T, resolved bool) *serviceFixture {
	t.Helper()
	return newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		reconcileThreads: []githubapp.ReviewThread{
			threadCarrying(t, claimThreadFinding(), resolved, ""),
		},
		model: &sequenceModel{results: []domain.ReviewResult{{
			Findings: []domain.Finding{
				claimTextRestatement("Publish error goes unchecked", 4, "third", 9),
			},
		}}},
	})
}

// A claim an open thread already carries must not come back on another line
// under another title. The thread anchors on line 2 and quotes one source line;
// the restatement anchors on line 4 and quotes another, so the anchor and the
// claim key both pass it and the claimtext hash in the marker is what catches it.
func TestARestatementOfAnOpenThreadsClaimIsWithheld(t *testing.T) {
	bodies := publishedBodies(t, claimThreadFixture(t, false))

	if len(bodies) != 0 {
		t.Fatalf("published comments = %v, want none: this defect is already open under another title", bodies)
	}
}

// A resolved thread is a settled question, so the same claim publishes again.
func TestARestatementOfAResolvedThreadsClaimIsPublished(t *testing.T) {
	bodies := publishedBodies(t, claimThreadFixture(t, true))

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: a resolved thread suppresses nothing", bodies)
	}
}

// Every comment already on an open pull request was published before the
// claimtext field existed, so its marker has none. Such a thread must suppress
// nothing by claim text: the candidate here names a defect, anchors two lines
// away, and quotes a different source line, so the keyless thread has nothing
// left to match it on and the finding reaches the page.
func TestAThreadWithNoClaimTextKeySuppressesNothingByClaimText(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		reconcileThreads: []githubapp.ReviewThread{
			threadCarrying(t, keylessStandingFinding(), false, ""),
		},
		model: &sequenceModel{results: []domain.ReviewResult{{
			Findings: []domain.Finding{
				claimTextRestatement("Publish error goes unchecked", 5, "fifth", 9),
			},
		}}},
		collector: fiveLineCollector{},
	})

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: a marker with no claimtext matches nothing", bodies)
	}
}

// fiveLineCollector adds five changed lines to one file, so a finding can
// anchor clear of a thread that spans the first few.
type fiveLineCollector struct{}

func (fiveLineCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	patch := strings.Join([]string{
		"@@ -1,1 +1,6 @@",
		" package main",
		"+added",
		"+second",
		"+third",
		"+fourth",
		"+fifth",
	}, "\n")
	changed, hunks, err := diff.ChangedRightLines(patch)
	if err != nil {
		return diff.ReviewInput{}, err
	}
	return diff.ReviewInput{
		PullRequest: pullRequest,
		Files: []diff.FileContext{{
			Path:              "main.go",
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    "package main\nadded\nsecond\nthird\nfourth\nfifth\n",
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		}},
	}, nil
}

// unrelatedRestatements are two findings the deterministic layers cannot join:
// different titles, different quoted lines, different claim sentences, and
// anchors two lines apart. Only reading them says they are one defect.
func unrelatedRestatements() []domain.Finding {
	return []domain.Finding{
		{
			Path:       "main.go",
			StartLine:  2,
			EndLine:    2,
			Title:      "Publish failure is ignored",
			Body:       "The failure of this call is not handled.",
			Evidence:   "added",
			Claim:      "The publish error is ignored",
			Suggestion: "",
			Importance: 9,
		},
		{
			Path:       "main.go",
			StartLine:  4,
			EndLine:    4,
			Title:      "Return value goes unchecked",
			Body:       "Nothing reads what this call returns.",
			Evidence:   "third",
			Claim:      "The publish return value is discarded",
			Suggestion: "",
			Importance: 10,
		},
	}
}

// equallyRatedRestatements are the same two findings rated the same, so
// importance decides nothing between them and the tie-break decides alone.
func equallyRatedRestatements() []domain.Finding {
	findings := unrelatedRestatements()
	findings[1].Importance = findings[0].Importance
	return findings
}

// consolidationFixture wires a run whose one chunk holds two restatements, with
// a scripted grouping and a scripted failure.
func consolidationFixture(
	t *testing.T,
	findings []domain.Finding,
	grouping []review.Consolidation,
	callErr error,
) (*serviceFixture, *sequenceModel) {
	t.Helper()
	model := &sequenceModel{
		results: []domain.ReviewResult{{
			Findings: findings,
		}},
		consolidations: grouping,
		consolidateErr: callErr,
	}
	return newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model:             model,
	}), model
}

// Two restatements that share no key are the gap every exact comparison leaves,
// and reading them is the only thing that closes it. The group merges to its
// strongest member, so the reader gets the more important of the two.
func TestAConsolidationGroupPublishesItsStrongestMember(t *testing.T) {
	fixture, model := consolidationFixture(t, unrelatedRestatements(), []review.Consolidation{{
		Groups: []review.ConsolidationGroup{{
			Candidates:         []int{1, 2},
			RestatesOpenThread: false,
			Reason:             "Both are the unchecked publish call.",
		}},
	}}, nil)

	bodies := publishedBodies(t, fixture)

	if model.consolidationCalls() != 1 {
		t.Fatalf("consolidation calls = %d, want exactly one for this chunk", model.consolidationCalls())
	}
	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: the model grouped them", bodies)
	}
	if !strings.Contains(bodies[0], "Return value goes unchecked") {
		t.Fatalf("published comment = %q, want the higher rated member of the group", bodies[0])
	}
}

// A group whose members are rated the same keeps the earlier finding, and which
// one that is must not depend on the order the model happened to list them in.
//
// The group here arrives as [2, 1], which is the order that exposes it: seeding
// the survivor from the first number listed and replacing only on strictly
// higher importance keeps 2, so the answer's phrasing decides which finding a
// reader sees. Every group in every other test arrives ascending, where the two
// rules agree, so none of them can catch this.
func TestAConsolidationGroupOfEqualsKeepsTheEarlierFindingWhateverTheOrder(t *testing.T) {
	fixture, _ := consolidationFixture(t, equallyRatedRestatements(), []review.Consolidation{{
		Groups: []review.ConsolidationGroup{{
			Candidates:         []int{2, 1},
			RestatesOpenThread: false,
			Reason:             "Both are the unchecked publish call.",
		}},
	}}, nil)

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: the model grouped them", bodies)
	}
	if !strings.Contains(bodies[0], "Publish failure is ignored") {
		t.Fatalf("published comment = %q, want the earlier finding: the two are rated the same", bodies[0])
	}
}

// A group marked as restating an open thread loses every member, because the
// thread is where that conversation already is.
func TestAConsolidationGroupThatRestatesAnOpenThreadPublishesNothing(t *testing.T) {
	fixture, _ := consolidationFixture(t, unrelatedRestatements(), []review.Consolidation{{
		Groups: []review.ConsolidationGroup{{
			Candidates:         []int{1, 2},
			RestatesOpenThread: true,
			Reason:             "This is the thread already open on the publish call.",
		}},
	}}, nil)

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 0 {
		t.Fatalf("published comments = %v, want none: both restate an open thread", bodies)
	}
}

// A reviewer that loses findings to its own tidying is worse than one that
// repeats itself, so a failed call publishes what the deterministic layers left
// and says so in the log.
func TestAFailedConsolidationCallPublishesTheDeterministicResult(t *testing.T) {
	logs := &syncBuffer{}
	model := &sequenceModel{
		results: []domain.ReviewResult{{
			Findings: unrelatedRestatements(),
		}},
		consolidateErr: errors.New("the provider refused the grouping"),
	}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model:             model,
		logWriter:         logs,
	})

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 2 {
		t.Fatalf("published comments = %v, want both: a failed grouping withholds nothing", bodies)
	}
	if !strings.Contains(logs.String(), "consolidation call failed") {
		t.Fatalf("service log = %q, want the fallback named", logs.String())
	}
}

// An answer naming a candidate that was never shown is not about the findings
// it was asked about, so none of it is applied.
func TestAConsolidationAnswerNamingAnUnknownCandidateIsRefused(t *testing.T) {
	logs := &syncBuffer{}
	model := &sequenceModel{
		results: []domain.ReviewResult{{
			Findings: unrelatedRestatements(),
		}},
		consolidations: []review.Consolidation{{
			Groups: []review.ConsolidationGroup{{
				Candidates:         []int{1, 7},
				RestatesOpenThread: false,
				Reason:             "A number nobody was shown.",
			}},
		}},
	}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model:             model,
		logWriter:         logs,
	})

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 2 {
		t.Fatalf("published comments = %v, want both: an unapplicable grouping withholds nothing", bodies)
	}
	if !strings.Contains(logs.String(), "consolidation call failed") {
		t.Fatalf("service log = %q, want the refusal named", logs.String())
	}
}

// A lone candidate on a pull request carrying no open finding of the service's
// own has nothing to restate, so it pays for no extra call. This is the cost
// bound: the call reaches a single candidate chunk only where there is already
// an open thread for it to be measured against.
func TestAChunkHoldingOneCandidateWithNoOpenThreadMakesNoConsolidationCall(t *testing.T) {
	model := &sequenceModel{results: []domain.ReviewResult{{
		Findings: unrelatedRestatements()[:1],
	}}}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model:             model,
	})

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want the one finding", bodies)
	}
	if model.consolidationCalls() != 0 {
		t.Fatalf("consolidation calls = %d, want none: nothing is open for it to restate",
			model.consolidationCalls())
	}
}

// loneRestatementFixture wires the failure the probe found: a thread already
// open on one line, and a chunk whose single finding is that same defect at
// another line, in other words, under another title.
//
// Every deterministic layer passes it by construction. The wording defeats the
// claim text, the different quoted line defeats the claim key, the different
// anchor defeats the overlap, and the different title defeats the identity.
func loneRestatementFixture(t *testing.T, grouping []review.Consolidation) (*serviceFixture, *sequenceModel) {
	t.Helper()
	standing := domain.Finding{
		Path:       "main.go",
		StartLine:  2,
		EndLine:    2,
		Title:      "Standing claim",
		Body:       "The failure of this call is not handled.",
		Evidence:   "added",
		Claim:      "The publish error is ignored",
		Suggestion: "",
		Importance: 9,
	}
	model := &sequenceModel{
		results: []domain.ReviewResult{{
			Findings: []domain.Finding{{
				Path:       "main.go",
				StartLine:  5,
				EndLine:    5,
				Title:      "Unchecked publish result",
				Body:       "Nothing reacts when this call fails.",
				Evidence:   "fifth",
				Claim:      "Publish failures are swallowed silently",
				Suggestion: "",
				Importance: 9,
			}},
		}},
		consolidations: grouping,
	}
	return newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		collector:         fiveLineCollector{},
		reconcileThreads: []githubapp.ReviewThread{
			threadCarrying(t, standing, false, ""),
		},
		model: model,
	}), model
}

// A chunk whose only candidate restates an open thread must still be asked
// about, because a lone candidate is exactly the shape no deterministic layer
// can catch and the shape agoodkind/tack 169 took five times.
//
// A probe published this finding before the gate reached a single candidate.
func TestALoneRestatementOfAnOpenThreadIsWithheld(t *testing.T) {
	fixture, model := loneRestatementFixture(t, []review.Consolidation{{
		Groups: []review.ConsolidationGroup{{
			Candidates:         []int{1},
			RestatesOpenThread: true,
			Reason:             "This is the open thread's claim in other words.",
		}},
	}})

	bodies := publishedBodies(t, fixture)

	if model.consolidationCalls() != 1 {
		t.Fatalf("consolidation calls = %d, want one: a thread is open for this candidate to restate",
			model.consolidationCalls())
	}
	if len(bodies) != 0 {
		t.Fatalf("published comments = %v, want none: this defect is already open under another title", bodies)
	}
}

// The open threads have to reach the call, not just trigger it, or the model is
// asked to compare a candidate against nothing.
func TestTheLoneCandidateCallCarriesTheOpenThread(t *testing.T) {
	fixture, model := loneRestatementFixture(t, nil)

	publishedBodies(t, fixture)

	if len(model.consolidatePrompts) != 1 {
		t.Fatalf("consolidation prompts = %d, want one", len(model.consolidatePrompts))
	}
	for _, want := range []string{"Open finding", "Standing claim", "Unchecked publish result"} {
		if !strings.Contains(model.consolidatePrompts[0], want) {
			t.Fatalf("consolidation prompt missing %q: %q", want, model.consolidatePrompts[0])
		}
	}
}

// The floor holds under the wider gate: a chunk that produced nothing publishable
// pays nothing, open threads or not, and that is most chunks of most deltas.
func TestAChunkWithNoCandidatesMakesNoConsolidationCall(t *testing.T) {
	standing := domain.Finding{
		Path:       "main.go",
		StartLine:  2,
		EndLine:    2,
		Title:      "Standing claim",
		Body:       "The failure of this call is not handled.",
		Evidence:   "added",
		Claim:      "The publish error is ignored",
		Suggestion: "",
		Importance: 9,
	}
	model := &sequenceModel{results: []domain.ReviewResult{{
		Findings: nil,
	}}}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		collector:         fiveLineCollector{},
		reconcileThreads: []githubapp.ReviewThread{
			threadCarrying(t, standing, false, ""),
		},
		model: model,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if model.consolidationCalls() != 0 {
		t.Fatalf("consolidation calls = %d, want none: the chunk holds nothing to ask about",
			model.consolidationCalls())
	}
}

// barrierConsolidationModel holds every consolidation call until both chunks
// have reached one, then releases both at once.
//
// That is the window the consolidation call opens. It holds no pass lock, so
// two chunks can each finish testing their candidates against what the run has
// carried, both find nothing, and both come out of the call together with the
// same claim in hand. Nothing here is contrived: chunks answer four at a time
// in production, and the two calls are the same length.
type barrierConsolidationModel struct {
	width   int
	release chan struct{}
	mu      sync.Mutex
	arrived int
	expired bool
}

// barrierWait bounds how long a call waits for the others. The barrier only
// closes when as many calls arrive as the test expects, so a change that stops
// a chunk producing two candidates would otherwise leave every call parked for
// as long as the suite is allowed to run. A hang reads as an infrastructure
// problem and gets rerun; a failure naming what it waited for reads as the
// defect it is.
const barrierWait = 10 * time.Second

func newBarrierConsolidationModel(width int) *barrierConsolidationModel {
	return &barrierConsolidationModel{
		width:   width,
		release: make(chan struct{}),
		mu:      sync.Mutex{},
		arrived: 0,
		expired: false,
	}
}

// arrivedCount reports how many calls reached the barrier, so a test that timed
// out can say how far short it fell.
func (model *barrierConsolidationModel) arrivedCount() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.arrived
}

// timedOut reports whether any call gave up waiting.
//
// The bound alone is not enough to make a broken premise visible. A call that
// gives up returns an error, the run publishes what the deterministic layers
// left, and the comment count a test asserts can come out right anyway, so the
// test would pass ten seconds slower and prove nothing. Asking whether the
// chunks actually met is what keeps it a test of the window.
func (model *barrierConsolidationModel) timedOut() bool {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.expired
}

func (model *barrierConsolidationModel) Review(
	_ context.Context,
	prompt string,
) (review.Completion, error) {
	return review.Completion{
		Result: domain.ReviewResult{
			Findings: sharedAndUniqueFindings(promptFilePath(prompt)),
		},
		Model: testReviewModel,
	}, nil
}

func (model *barrierConsolidationModel) Consolidate(
	_ context.Context,
	_ string,
) (review.Consolidation, error) {
	model.mu.Lock()
	model.arrived++
	last := model.arrived == model.width
	model.mu.Unlock()
	if last {
		close(model.release)
	}
	select {
	case <-model.release:
		return review.Consolidation{Groups: nil}, nil
	case <-time.After(barrierWait):
		model.mu.Lock()
		model.expired = true
		seen := model.arrived
		model.mu.Unlock()
		return review.Consolidation{}, fmt.Errorf(
			"consolidation barrier waited %s for %d calls and saw %d",
			barrierWait,
			model.width,
			seen,
		)
	}
}

// sharedAndUniqueFindings is one chunk's answer: the defect both chunks name in
// the first file, and one only this chunk's own file has. Two candidates is what
// asks for a consolidation call at all.
//
// The shared claim names file0.go from both chunks, because the claim sentence
// only joins two findings about one file. Each chunk quotes its own line of it,
// so the claim key and the anchor both pass them and the sentence is what is
// under test.
func sharedAndUniqueFindings(path string) []domain.Finding {
	suffix := strings.TrimSuffix(strings.TrimPrefix(path, "file"), ".go")
	sharedLine := 2
	sharedEvidence := sharedClaimLine
	if path != "file0.go" {
		sharedLine = 3
		sharedEvidence = "other0"
	}
	return []domain.Finding{
		{
			Path:       "file0.go",
			StartLine:  sharedLine,
			EndLine:    sharedLine,
			Title:      "Publish failure seen from " + path,
			Body:       "The failure of this call is not handled.",
			Evidence:   sharedEvidence,
			Claim:      sharedClaimSentence,
			Suggestion: "",
			Importance: 9,
		},
		{
			Path:       path,
			StartLine:  4,
			EndLine:    4,
			Title:      "Unused helper in " + path,
			Body:       "Nothing in this file calls the helper it adds.",
			Evidence:   "third" + suffix,
			Claim:      "The helper added to " + path + " has no caller",
			Suggestion: "",
			Importance: 9,
		},
	}
}

// Two chunks released from their consolidation calls at the same instant both
// hold the same claim, and both have already been told nobody carries it. Only
// the test the render stage repeats, inside the same lock hold that records
// what a chunk carried, keeps that down to one comment.
func TestTwoChunksLeavingConsolidationTogetherPublishOneSharedClaim(t *testing.T) {
	model := newBarrierConsolidationModel(2)
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         twoFileChunkCollector{},
		minimumImportance: 9,
		model:             model,
	})

	bodies := publishedBodies(t, fixture)

	// The two chunks have to actually meet inside the call, or this proves
	// nothing about the window: a call that gave up publishes the deterministic
	// result, which can carry the same comment count for the wrong reason.
	if model.timedOut() {
		t.Fatalf("consolidation barrier saw %d of %d calls: the two chunks never met inside it",
			model.arrivedCount(), 2)
	}
	if len(bodies) != 3 {
		t.Fatalf("published comments = %d, want three: one shared claim and one defect per file:\n%v",
			len(bodies), bodies)
	}
	shared := 0
	for _, body := range bodies {
		if strings.Contains(body, "Publish failure seen from") {
			shared++
		}
	}
	if shared != 1 {
		t.Fatalf("comments naming the shared defect = %d, want 1", shared)
	}
}

// neighbouringRestatement is a finding anchored over a range that overlaps the
// standing thread's, resting on a line the thread never quoted.
func neighbouringRestatement() domain.Finding {
	return domain.Finding{
		Path:       "main.go",
		StartLine:  3,
		EndLine:    4,
		Title:      "Neighbouring restatement",
		Body:       "The same region the open thread already objects to.",
		Evidence:   "third",
		Suggestion: "",
		Importance: 9,
	}
}

// keylessStandingFinding is a finding as it was published before claim keys
// existed: no evidence, so its marker carries no key and only its anchor can
// recognize a restatement of it.
func keylessStandingFinding() domain.Finding {
	return domain.Finding{
		Path:       "main.go",
		StartLine:  2,
		EndLine:    3,
		Title:      "Standing defect",
		Body:       "The changed lines break core behavior.",
		Evidence:   "",
		Suggestion: "",
		Importance: 9,
	}
}

// standingThreadFixture wires a run whose one chunk restates a claim a thread
// already carries, anchored one line over.
func standingThreadFixture(t *testing.T, resolved bool) *serviceFixture {
	t.Helper()
	return newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		reconcileThreads: []githubapp.ReviewThread{
			threadCarrying(t, keylessStandingFinding(), resolved, ""),
		},
		model: &sequenceModel{results: []domain.ReviewResult{{
			Findings: []domain.Finding{neighbouringRestatement()},
		}}},
	})
}

// A restatement anchored one line over is the case the anchor history missed:
// it compared exact end lines, so a thread on lines 2 to 3 and a finding on
// lines 3 to 4 looked like two separate claims. They share line 3.
//
// The standing thread carries no claim key, which is what every comment
// published before the key existed carries, so the overlap is the only thing
// that can recognize this.
func TestARestatementOverlappingAnOpenThreadIsWithheld(t *testing.T) {
	bodies := publishedBodies(t, standingThreadFixture(t, false))

	if len(bodies) != 0 {
		t.Fatalf("published comments = %v, want none: the open thread already objects to line 3", bodies)
	}
}

// A resolved thread is a settled question. A defect reintroduced after a fix
// has to be raised again, so no resolved thread withholds anything by overlap.
func TestARestatementOverlappingAResolvedThreadIsPublished(t *testing.T) {
	bodies := publishedBodies(t, standingThreadFixture(t, true))

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: a resolved thread suppresses nothing", bodies)
	}
	if !strings.Contains(bodies[0], "Neighbouring restatement") {
		t.Fatalf("published comment = %q, want the finding raised again", bodies[0])
	}
}

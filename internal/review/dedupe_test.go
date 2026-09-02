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
			CoverageComplete: true,
			Findings:         findings,
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
			"@@ -1,1 +1,3 @@\n line%d\n+%s\n+other%d\n",
			index,
			sharedClaimLine,
			index,
		)
		changed, hunks, err := diff.ChangedRightLines(patch)
		if err != nil {
			return diff.ReviewInput{}, err
		}
		files = append(files, diff.FileContext{
			Path:              fmt.Sprintf("file%d.go", index),
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    padding + sharedClaimLine + "\n",
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
		CoverageComplete: true,
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

// crossChunkClaimText is one chunk's answer: the shared claim sentence, worded
// its own way and resting on that chunk's own file.
func crossChunkClaimText(title string, path string) domain.ReviewResult {
	return domain.ReviewResult{
		CoverageComplete: true,
		Findings: []domain.Finding{{
			Path:       path,
			StartLine:  2,
			EndLine:    2,
			Title:      title,
			Body:       "The failure of this call is not handled.",
			Evidence:   sharedClaimLine,
			Claim:      sharedClaimSentence,
			Suggestion: "",
			Importance: 9,
		}},
	}
}

// No chunk sees another chunk's answer, so two chunks naming one defect on two
// files is the cross-run failure happening inside a single run. The two quote
// the same line of two different files, so their claim keys differ on the path
// and only the claim sentence joins them.
func TestTwoChunksNamingOneDefectPublishItOnce(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         twoFileChunkCollector{},
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{
			crossChunkClaimText("Publish failure is ignored", "file0.go"),
			crossChunkClaimText("Publish error goes unchecked", "file1.go"),
		}},
	})

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 1 {
		t.Fatalf("published comments = %v, want one: both chunks name the same defect", bodies)
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
			CoverageComplete: true,
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
			CoverageComplete: true,
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

// consolidationFixture wires a run whose one chunk holds the two restatements,
// with a scripted grouping and a scripted failure.
func consolidationFixture(
	t *testing.T,
	grouping []review.Consolidation,
	callErr error,
) (*serviceFixture, *sequenceModel) {
	t.Helper()
	model := &sequenceModel{
		results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings:         unrelatedRestatements(),
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
	fixture, model := consolidationFixture(t, []review.Consolidation{{
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

// A group marked as restating an open thread loses every member, because the
// thread is where that conversation already is.
func TestAConsolidationGroupThatRestatesAnOpenThreadPublishesNothing(t *testing.T) {
	fixture, _ := consolidationFixture(t, []review.Consolidation{{
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
			CoverageComplete: true,
			Findings:         unrelatedRestatements(),
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
			CoverageComplete: true,
			Findings:         unrelatedRestatements(),
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

// One candidate can restate nothing, so a chunk holding one pays for no extra
// call. The call is bounded per chunk, never per candidate.
func TestAChunkHoldingOneCandidateMakesNoConsolidationCall(t *testing.T) {
	model := &sequenceModel{results: []domain.ReviewResult{{
		CoverageComplete: true,
		Findings:         unrelatedRestatements()[:1],
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
		t.Fatalf("consolidation calls = %d, want none for a single candidate", model.consolidationCalls())
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
}

func newBarrierConsolidationModel(width int) *barrierConsolidationModel {
	return &barrierConsolidationModel{
		width:   width,
		release: make(chan struct{}),
		mu:      sync.Mutex{},
		arrived: 0,
	}
}

func (model *barrierConsolidationModel) Review(
	_ context.Context,
	prompt string,
) (review.Completion, error) {
	return review.Completion{
		Result: domain.ReviewResult{
			CoverageComplete: true,
			Findings:         sharedAndUniqueFindings(promptFilePath(prompt)),
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
	<-model.release
	return review.Consolidation{Groups: nil}, nil
}

// sharedAndUniqueFindings is one chunk's answer: the defect both chunks name,
// and one only this chunk's file has. Two candidates is what asks for a
// consolidation call at all.
func sharedAndUniqueFindings(path string) []domain.Finding {
	return []domain.Finding{
		{
			Path:       path,
			StartLine:  2,
			EndLine:    2,
			Title:      "Publish failure in " + path,
			Body:       "The failure of this call is not handled.",
			Evidence:   sharedClaimLine,
			Claim:      sharedClaimSentence,
			Suggestion: "",
			Importance: 9,
		},
		{
			Path:       path,
			StartLine:  3,
			EndLine:    3,
			Title:      "Unused helper in " + path,
			Body:       "Nothing in this file calls the helper it adds.",
			Evidence:   "other" + strings.TrimSuffix(strings.TrimPrefix(path, "file"), ".go"),
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
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         twoFileChunkCollector{},
		minimumImportance: 9,
		model:             newBarrierConsolidationModel(2),
	})

	bodies := publishedBodies(t, fixture)

	if len(bodies) != 3 {
		t.Fatalf("published comments = %d, want three: one shared claim and one defect per file:\n%v",
			len(bodies), bodies)
	}
	shared := 0
	for _, body := range bodies {
		if strings.Contains(body, "Publish failure in") {
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
			CoverageComplete: true,
			Findings:         []domain.Finding{neighbouringRestatement()},
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

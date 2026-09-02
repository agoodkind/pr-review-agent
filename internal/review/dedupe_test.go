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
	"fmt"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

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

package review_test

// These tests cover the head this service cannot read whole: a hunk larger than
// one model request, which no later run will fare any better on.
//
// The transient case has the opposite ending and is covered here too, because
// the two are one sentence apart on the pull request and it is the promise of a
// next push that separates them.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/review"
)

const (
	// coverageReadablePath is the file whose one hunk fits in a model request.
	coverageReadablePath = "readable.go"
	// coverageUnreadablePath is the file whose one hunk does not.
	coverageUnreadablePath = "huge.go"
	// coverageOversizedLines is enough added lines to put one hunk past the
	// maximum prompt size on its own.
	coverageOversizedLines = 2100
	// coveragePaddingWidth is how wide each added line of that hunk is.
	coveragePaddingWidth = 60
	// coveragePriorHead is a commit an earlier run reviewed, so the tests can
	// tell a baseline that was held from one that was never set.
	coveragePriorHead = "1111111111111111111111111111111111111111"
	// nextPushSentence is the promise the transient ending makes and the
	// structural ending must never make.
	nextPushSentence = "The next push reviews"
	// unreviewedHeadSentence is the one reason a head used to be blocked by when
	// the model answered coverage blind.
	unreviewedHeadSentence = "This head was not fully reviewed"
)

// coveragePatch renders one hunk adding addedLines lines after a single line of
// context, which is the shape every file in these tests carries.
func coveragePatch(addedLines int) string {
	lines := make([]string, 0, addedLines+2)
	lines = append(lines, fmt.Sprintf("@@ -1,1 +1,%d @@", addedLines+1), " package main")
	for index := range addedLines {
		lines = append(lines, "+"+coverageAddedLine(index))
	}
	return strings.Join(lines, "\n")
}

// coverageContent is the file as it stands after that hunk applies.
func coverageContent(addedLines int) string {
	lines := make([]string, 0, addedLines+1)
	lines = append(lines, "package main")
	for index := range addedLines {
		lines = append(lines, coverageAddedLine(index))
	}
	return strings.Join(lines, "\n") + "\n"
}

func coverageAddedLine(index int) string {
	if index == 0 {
		return "added"
	}
	return fmt.Sprintf("pad%d%s", index, strings.Repeat("x", coveragePaddingWidth))
}

// oversizedHunkCollector returns one file whose single hunk is larger than one
// whole model request, beside one file a model can read.
type oversizedHunkCollector struct{}

func (oversizedHunkCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	specs := []struct {
		path       string
		addedLines int
	}{
		{path: coverageReadablePath, addedLines: 1},
		{path: coverageUnreadablePath, addedLines: coverageOversizedLines},
	}
	files := make([]diff.FileContext, 0, len(specs))
	for _, spec := range specs {
		patch := coveragePatch(spec.addedLines)
		changed, hunks, err := diff.ChangedRightLines(patch)
		if err != nil {
			return diff.ReviewInput{}, err
		}
		files = append(files, diff.FileContext{
			Path:              spec.path,
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    coverageContent(spec.addedLines),
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
			Gap:               diff.CoverageGapNone,
		})
	}
	return diff.ReviewInput{PullRequest: pullRequest, Files: files, MergeBase: ""}, nil
}

// coverageFinding is the defect the model reports in the hunk it could read.
func coverageFinding() domain.ReviewResult {
	return domain.ReviewResult{
		Findings: []domain.Finding{{
			Path:       coverageReadablePath,
			StartLine:  2,
			EndLine:    2,
			Title:      "Severe defect",
			Body:       "The changed line breaks core behavior.",
			Evidence:   "added",
			Suggestion: "",
			Importance: 9,
		}},
	}
}

// readableOnlyCollector returns one small file a model reads whole, which is
// the shape of a pull request with nothing structurally wrong with it.
type readableOnlyCollector struct{}

func (readableOnlyCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	patch := coveragePatch(1)
	changed, hunks, err := diff.ChangedRightLines(patch)
	if err != nil {
		return diff.ReviewInput{}, err
	}
	return diff.ReviewInput{
		PullRequest: pullRequest,
		Files: []diff.FileContext{{
			Path:              coverageReadablePath,
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    coverageContent(1),
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
			Gap:               diff.CoverageGapNone,
		}},
		MergeBase: "",
	}, nil
}

// seedReviewedBaseline puts a completed earlier run's checkpoint on the pull
// request, so a test can tell a baseline that was held from one never set.
func seedReviewedBaseline(fixture *serviceFixture) {
	seedStateNaming(fixture, coveragePriorHead)
}

// seedStateNaming puts a completed run's checkpoint on the pull request, naming
// the commit that run read whole.
func seedStateNaming(fixture *serviceFixture, reviewed string) {
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id": float64(2000),
		"body": "## Review\n" + marker.EncodeState(marker.State{
			LastReviewed: domain.HeadSHA(reviewed),
			RunID:        "delivery-0",
			Status:       marker.StateDone,
			Pending:      nil,
			Completed:    nil,
		}),
		"user": map[string]any{"login": testBotLogin},
	})
}

// A clean run approves, whatever a model answer says about coverage.
//
// This is the live symptom on pull request 91 at head 0f1cccf, run
// bd5315d0-a705-11f1-8ece-8f2f914eb999, and on 92 at head cd0efc5, whose only
// change is one test file. Both had every chunk complete, no chunk failure, no
// finding, and nothing structural, and both blocked with the single reason that
// the head was not fully reviewed. The schema required a coverage_complete
// boolean the prompt never explained, the model filled it blind, and one false
// answer set the whole pass incomplete. Coverage is this service's own
// observation now, so nothing a model answers about it decides anything.
func TestACleanRunApprovesWhateverTheModelSaysAboutCoverage(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         readableOnlyCollector{},
		minimumImportance: 9,
		model:             &sequenceModel{results: []domain.ReviewResult{{Findings: nil}}},
	})
	seedReviewedBaseline(fixture)

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("event = %v, want APPROVE over a head this run read whole",
			fixture.state.lastSubmitReview["event"])
	}
	body := failureSummaryComment(t, fixture)
	if strings.Contains(body, unreviewedHeadSentence) {
		t.Fatalf("summary blocks a head with nothing unread:\n%s", body)
	}
	if !strings.Contains(body, "| Coverage complete | yes |") {
		t.Fatalf("summary reports incomplete coverage on a run that read everything:\n%s", body)
	}
	state := decodedSummaryState(t, fixture)
	if state.LastReviewed != domain.HeadSHA(testHeadSHA) {
		t.Fatalf("last reviewed = %q, want the head this run read whole", state.LastReviewed)
	}
}

// A resolve delivery at a head an earlier run recorded as read whole approves,
// even when that run's own verdict body still carries the old blind sentence.
//
// Every head blocked by the coverage the model used to answer carries exactly
// that combination, so believing the body rather than the checkpoint would hold
// those blocks standing on a fact that was never true.
func TestAResolutionTrustsTheCheckpointOverAStaleVerdictSentence(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reviewPages: [][]map[string]any{{{
			"id":        float64(4100),
			"commit_id": testHeadSHA,
			"state":     "CHANGES_REQUESTED",
			// The whole sentence, because that is what the refresh used to test
			// the body for. A fragment would pass whichever signal decided.
			"body": "Changes requested.\n\nWaiting on:\n- " + testUnreviewedHeadReason + "\n\n" +
				marker.Review(domain.HeadSHA(testHeadSHA), domain.ReviewDecisionRequestChanges),
			"user": map[string]any{"login": testBotLogin},
		}}},
	})
	seedStateNaming(fixture, testHeadSHA)

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("event = %v, want the stale block cleared once every thread is resolved",
			fixture.state.lastSubmitReview["event"])
	}
}

// A hunk nobody got an answer about survives the run that found it.
//
// The shortfall the truncation path records lives in the pass, which dies with
// the process. If the chunk that produced it were recorded as read, the next run
// would subtract it from the delta, find nothing left to review, see no
// shortfall, and advance the baseline over code nobody has ever read. One run
// cannot show that. The second one can.
func TestAnUnreadChunkIsNotRecordedAsReadSoALaterRunStillHoldsTheBaseline(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         multiHunkCollector{},
		minimumImportance: 9,
		model:             &truncatedModel{truncateCalls: 1000},
	})
	seedReviewedBaseline(fixture)

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	first := decodedSummaryState(t, fixture)
	if len(first.Completed) != 0 {
		t.Fatalf("completed after the first run = %v, want no chunk recorded as read",
			first.Completed)
	}
	if first.LastReviewed != domain.HeadSHA(coveragePriorHead) {
		t.Fatalf("last reviewed after the first run = %q, want the held baseline",
			first.LastReviewed)
	}

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	second := decodedSummaryState(t, fixture)
	if second.LastReviewed != domain.HeadSHA(coveragePriorHead) {
		t.Fatalf("last reviewed after the second run = %q, want the baseline still held over hunks nobody read",
			second.LastReviewed)
	}
	if len(fixture.state.submittedReviews) != 0 {
		t.Fatalf("submitted reviews = %v, want none from either run",
			fixture.state.submittedReviews)
	}
	if conclusion := fixture.state.lastUpdateCheckRun["conclusion"]; conclusion != "action_required" {
		t.Fatalf("check conclusion after the second run = %v, want action_required", conclusion)
	}
}

// A hunk larger than one model request is not a temporary shortfall. The run
// reports every defect it found in the hunks it could read, submits no verdict
// for anyone to dismiss, holds the merge gate, names the hunk nobody read, and
// leaves the durable baseline exactly where the last completed run left it.
func TestAHunkTooLargeToReadSubmitsNoVerdictAndHoldsTheBaseline(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         oversizedHunkCollector{},
		minimumImportance: 9,
		model:             &sequenceModel{results: []domain.ReviewResult{coverageFinding()}},
	})
	seedReviewedBaseline(fixture)

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fixture.state.submittedReviews) != 0 {
		t.Fatalf("submitted reviews = %v, want none over a head nobody read whole",
			fixture.state.submittedReviews)
	}
	if len(fixture.state.dismissals) != 0 {
		t.Fatalf("dismissals = %v, want no review object touched", fixture.state.dismissals)
	}

	// The findings from the hunks that did read are real whatever else went
	// unread, so they still reach the pull request.
	if len(fixture.state.streamedComments) != 1 {
		t.Fatalf("streamed comments = %d, want the one finding from the readable hunk",
			len(fixture.state.streamedComments))
	}
	if path := fmt.Sprint(fixture.state.streamedComments[0]["path"]); path != coverageReadablePath {
		t.Fatalf("streamed comment path = %q, want %q", path, coverageReadablePath)
	}

	if conclusion := fixture.state.lastUpdateCheckRun["conclusion"]; conclusion != "action_required" {
		t.Fatalf("check conclusion = %v, want action_required holding the gate", conclusion)
	}
	output := checkOutput(t, fixture)
	summary := fmt.Sprint(output["summary"])
	if !strings.Contains(summary, coverageUnreadablePath) {
		t.Fatalf("check summary = %q, want the path of the hunk nobody read", summary)
	}
	if !strings.Contains(summary, "split the pull request") {
		t.Fatalf("check summary = %q, want what a person has to do about it", summary)
	}
	if strings.Contains(summary, nextPushSentence) {
		t.Fatalf("check summary = %q, want no promise a later run cannot keep", summary)
	}

	body := failureSummaryComment(t, fixture)
	if !strings.Contains(body, coverageUnreadablePath) {
		t.Fatalf("summary comment = %q, want the path of the hunk nobody read", body)
	}
	if strings.Contains(body, nextPushSentence) {
		t.Fatalf("summary comment = %q, want no promise a later run cannot keep", body)
	}

	state := decodedSummaryState(t, fixture)
	if state.LastReviewed != domain.HeadSHA(coveragePriorHead) {
		t.Fatalf("last reviewed = %q, want the baseline the last completed run left",
			state.LastReviewed)
	}
}

// The same head delivered again measures the same range again. A baseline that
// advanced would let the next run report the head as already reviewed, and the
// hunk nobody read would be behind the checkpoint for good.
func TestARedeliveredUnreadableHeadIsHeldAgainRatherThanReportedReviewed(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         oversizedHunkCollector{},
		minimumImportance: 9,
		model:             &sequenceModel{results: []domain.ReviewResult{coverageFinding()}},
	})
	seedReviewedBaseline(fixture)

	for attempt := 1; attempt <= 2; attempt++ {
		if err := fixture.run(context.Background(), fixture.job()); err != nil {
			t.Fatalf("Run %d: %v", attempt, err)
		}
	}

	if conclusion := fixture.state.lastUpdateCheckRun["conclusion"]; conclusion != "action_required" {
		t.Fatalf("check conclusion after the redelivery = %v, want action_required", conclusion)
	}
	title := fmt.Sprint(checkOutput(t, fixture)["title"])
	if strings.Contains(title, "Already reviewed") {
		t.Fatalf("check title = %q, want the head still held rather than reported reviewed", title)
	}
	if !strings.Contains(title, "cannot be reviewed") {
		t.Fatalf("check title = %q, want the count of what nobody could read", title)
	}
	if len(fixture.state.submittedReviews) != 0 {
		t.Fatalf("submitted reviews = %v, want none from either run", fixture.state.submittedReviews)
	}
	state := decodedSummaryState(t, fixture)
	if state.LastReviewed != domain.HeadSHA(coveragePriorHead) {
		t.Fatalf("last reviewed = %q, want the baseline still held after the redelivery",
			state.LastReviewed)
	}
	// The chunks the first run read stay recorded, so the redelivery does not
	// pay a second time for work this pull request already bought.
	if len(state.Completed) == 0 {
		t.Fatal("completed chunks = none, want the chunks the first run read")
	}
}

// A chunk whose model call failed is the other kind of shortfall. It stays
// pending and keeps its promise, because the next push really does finish it.
func TestAFailedChunkKeepsTheNextPushPromise(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model:             &sequenceModel{err: errors.New("the provider did not answer")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	title := fmt.Sprint(checkOutput(t, fixture)["title"])
	if !strings.Contains(title, nextPushSentence) {
		t.Fatalf("check title = %q, want the promise the next push can keep", title)
	}
	state := decodedSummaryState(t, fixture)
	if len(state.Pending) != 1 {
		t.Fatalf("pending = %v, want the chunk that failed", state.Pending)
	}
	if state.Status != marker.StateReviewing {
		t.Fatalf("status = %q, want %q", state.Status, marker.StateReviewing)
	}
}

// A checkpoint about some other commit decides nothing about this head.
//
// A force push back to a commit already reviewed leaves the baseline sitting on
// the commit in between, while this head still carries the verdict the run that
// read it submitted. That verdict is the only witness to how much of this head
// was read, so it decides, and a valid approval is not replaced by a block
// nobody can clear.
func TestACheckpointAboutAnotherCommitLeavesTheVerdictBodyToDecide(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reviewPages: [][]map[string]any{{{
			"id":        float64(4100),
			"commit_id": testHeadSHA,
			"state":     "CHANGES_REQUESTED",
			"body": "Changes requested.\n\n" +
				marker.Review(domain.HeadSHA(testHeadSHA), domain.ReviewDecisionRequestChanges),
			"user": map[string]any{"login": testBotLogin},
		}}},
	})
	seedReviewedBaseline(fixture)

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("event = %v, want the approval the verdict at this head supports",
			fixture.state.lastSubmitReview["event"])
	}
}

// A durable state read that failed decides nothing and stops the refresh.
//
// A run that could not read the whole head submits no verdict, so its
// incompleteness exists only in the comment this read just failed on. Treating
// the failure as an absence would fall back to a body that says nothing about
// it, and approve a head nobody covered.
func TestAFailedStateReadStopsTheRefreshRatherThanApproving(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		issueCommentStatus: http.StatusInternalServerError,
		reviewPages: [][]map[string]any{{{
			"id":        float64(4100),
			"commit_id": testHeadSHA,
			"state":     "CHANGES_REQUESTED",
			"body": "Changes requested.\n\n" +
				marker.Review(domain.HeadSHA(testHeadSHA), domain.ReviewDecisionRequestChanges),
			"user": map[string]any{"login": testBotLogin},
		}}},
	})

	err := fixture.run(context.Background(), fixture.job())
	if err == nil {
		t.Fatal("Run: nil, want the failed state read reported rather than swallowed")
	}
	if len(fixture.state.submittedReviews) != 0 {
		t.Fatalf("submitted reviews = %v, want none decided from a read that failed",
			fixture.state.submittedReviews)
	}
}

// partiallyReadableModel answers exactly one call and truncates every other, so
// a chunk that splits comes back with findings from one half and nothing at all
// from the other.
type partiallyReadableModel struct {
	noConsolidation
	answerOnCall int
	calls        int
}

func (model *partiallyReadableModel) Review(
	_ context.Context,
	_ string,
) (review.Completion, error) {
	model.calls++
	if model.calls != model.answerOnCall {
		return review.Completion{}, &openai.TruncatedError{Model: testReviewModel}
	}
	return review.Completion{
		Result: domain.ReviewResult{Findings: []domain.Finding{{
			Path:       "main.go",
			StartLine:  2,
			EndLine:    2,
			Title:      "Severe defect",
			Body:       "The changed line breaks core behavior.",
			Evidence:   "added1",
			Suggestion: "",
			Importance: 9,
		}}},
		Model: testReviewModel,
	}, nil
}

// A chunk that answered in part is finished, and the head it left unread is
// still unread on every later run.
//
// This is the half of the shortfall that in-memory bookkeeping lost. A chunk
// whose split produced findings from one half and no answer at all from the
// other is not owed: re-reading it would pay for the half that did answer on
// every run forever. So it lands in the completed set, the next run subtracts
// that set from the delta and never re-derives it, and nothing it observes for
// itself says any part of this head went unread. Only the recorded id does, and
// without it the second run advances the baseline over code nobody has read.
//
// One run cannot show that, because the run that saw the shortfall still holds
// the baseline on what it remembers. The second one can.
func TestAPartlyReadChunkKeepsTheBaselineHeldOnTheNextRun(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         multiHunkCollector{},
		minimumImportance: 9,
		// The whole chunk truncates, splits, and the first half answers; the
		// second half truncates down to single hunks that cannot split again.
		model: &partiallyReadableModel{answerOnCall: 2},
	})
	seedReviewedBaseline(fixture)

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	first := decodedSummaryState(t, fixture)
	if len(first.Completed) == 0 {
		t.Fatalf("completed after the first run = %v, want the chunk that answered in part recorded as read",
			first.Completed)
	}
	if len(first.Unread) == 0 {
		t.Fatalf("unread after the first run = %v, want the chunk's shortfall recorded durably",
			first.Unread)
	}
	if first.LastReviewed != domain.HeadSHA(coveragePriorHead) {
		t.Fatalf("last reviewed after the first run = %q, want the held baseline", first.LastReviewed)
	}

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	second := decodedSummaryState(t, fixture)
	if second.LastReviewed != domain.HeadSHA(coveragePriorHead) {
		t.Fatalf("last reviewed after the second run = %q, want the baseline still held over a hunk nobody read",
			second.LastReviewed)
	}
	// Blocking here is right, because the half that answered found a real defect.
	// Approving would not be: part of this head has never been read.
	if event := fixture.state.lastSubmitReview["event"]; event == string(domain.ReviewDecisionApprove) {
		t.Fatal("the second run approved a head part of which nobody has read")
	}
}

// The recorded shortfall clears itself once the author rewrites that code.
//
// Chunk ids are content digests, so an id recorded against a hunk nobody could
// read matches nothing in a delta that no longer holds it. Keeping it past that
// would block the pull request forever on code that is gone, which is the one
// way this record could be worse than the bug it fixes.
func TestARecordedShortfallIsDroppedOnceItsChunkIsGone(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         readableOnlyCollector{},
		minimumImportance: 9,
		model:             &sequenceModel{results: []domain.ReviewResult{{Findings: nil}}},
	})
	// A checkpoint naming a shortfall against a chunk this delta does not hold.
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id": float64(2000),
		"body": "## Review\n" + marker.EncodeState(marker.State{
			LastReviewed: domain.HeadSHA(coveragePriorHead),
			RunID:        "delivery-0",
			Status:       marker.StateReviewing,
			Pending:      nil,
			Completed:    nil,
			ForcedBy:     "",
			Unread:       []string{"deadbeefdead"},
		}),
		"user": map[string]any{"login": testBotLogin},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	state := decodedSummaryState(t, fixture)
	if state.LastReviewed != domain.HeadSHA(testHeadSHA) {
		t.Fatalf("last reviewed = %q, want the baseline advanced once the unread chunk left the delta",
			state.LastReviewed)
	}
	if len(state.Unread) != 0 {
		t.Fatalf("unread = %v, want the stale id dropped", state.Unread)
	}
}

// A chunk that panics returns its worker slot, so later chunks can start.
func TestAPanickingChunkReturnsItsWorkerSlot(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector: manyChunkCollector{},
		model:     panicModel{},
	})

	// A timeout turns the former deadlock into a test failure.
	done := make(chan error, 1)
	go func() {
		done <- fixture.run(context.Background(), fixture.job())
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned no error, want the panic reported")
		}
		if !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("Run error = %v, want the chunk panic reported", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the run never returned: every worker slot was consumed by a panic and never given back")
	}
}

// Pending work from a later commit does not block an already reviewed head.
func TestPendingWorkFromALaterCommitDoesNotBlockAReviewedHead(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reviewPages: [][]map[string]any{{{
			"id":        float64(4100),
			"commit_id": testHeadSHA,
			"state":     "CHANGES_REQUESTED",
			"body": "Changes requested.\n\n" +
				marker.Review(domain.HeadSHA(testHeadSHA), domain.ReviewDecisionRequestChanges),
			"user": map[string]any{"login": testBotLogin},
		}}},
	})
	// This head was read whole, and a run on a commit after it left a chunk owed
	// before the branch was reset back here.
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id": float64(2000),
		"body": "## Review\n" + marker.EncodeState(marker.State{
			LastReviewed: domain.HeadSHA(testHeadSHA),
			RunID:        "delivery-0",
			Status:       marker.StateReviewing,
			Pending:      []string{"c0ffeec0ffee"},
			Completed:    nil,
			ForcedBy:     "",
			Unread:       nil,
		}),
		"user": map[string]any{"login": testBotLogin},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("event = %v, want the reviewed head approved once every thread is resolved",
			fixture.state.lastSubmitReview["event"])
	}
}

// A fully unread chunk advances after a later run reads it successfully.
func TestAFullyUnreadChunkClearsAfterASuccessfulRetry(t *testing.T) {
	model := &truncatedModel{truncateCalls: 1000}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         multiHunkCollector{},
		minimumImportance: 9,
		model:             model,
	})
	seedReviewedBaseline(fixture)

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	first := decodedSummaryState(t, fixture)
	if len(first.Pending) != 1 {
		t.Fatalf("pending after first run = %v, want one unread chunk", first.Pending)
	}
	if len(first.Unread) != 0 {
		t.Fatalf("unread after first run = %v, want the owed chunk tracked only as pending", first.Unread)
	}
	model.truncateCalls = model.calls

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	second := decodedSummaryState(t, fixture)
	if second.LastReviewed != domain.HeadSHA(testHeadSHA) {
		t.Fatalf("last reviewed = %q, want the head after the retry succeeded", second.LastReviewed)
	}
	if len(second.Pending) != 0 || len(second.Unread) != 0 {
		t.Fatalf("pending = %v, unread = %v, want no remaining work", second.Pending, second.Unread)
	}
}

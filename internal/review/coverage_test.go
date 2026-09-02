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
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
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
		CoverageComplete: true,
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

// seedReviewedBaseline puts a completed earlier run's checkpoint on the pull
// request, so a test can tell a baseline that was held from one never set.
func seedReviewedBaseline(fixture *serviceFixture) {
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id": float64(2000),
		"body": "## Review\n" + marker.EncodeState(marker.State{
			LastReviewed: domain.HeadSHA(coveragePriorHead),
			RunID:        "delivery-0",
			Status:       marker.StateDone,
			Pending:      nil,
			Completed:    nil,
		}),
		"user": map[string]any{"login": testBotLogin},
	})
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

// A thread resolution at a head no run read whole must not approve it.
//
// The verdict this used to be recovered from is the standing review's body, and
// a head this service never reviewed whole carries no verdict of its own to
// read. The durable baseline is the record that survives, so the refresh reads
// it and keeps requesting changes.
func TestAResolutionAtAnUnreadHeadSubmitsNoApproval(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reviewPages: [][]map[string]any{{{
			"id":        float64(4100),
			"commit_id": testHeadSHA,
			"state":     "CHANGES_REQUESTED",
			"body":      "Changes requested.\n\n" + marker.Review(domain.HeadSHA(testHeadSHA)),
			"user":      map[string]any{"login": testBotLogin},
		}}},
	})
	seedReviewedBaseline(fixture)

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, submitted := range fixture.state.submittedReviews {
		if submitted["state"] == "APPROVED" {
			t.Fatalf("submitted reviews = %v, want no approval at a head nobody read whole",
				fixture.state.submittedReviews)
		}
	}
	if fixture.state.lastSubmitReview != nil &&
		fixture.state.lastSubmitReview["event"] == string(domain.ReviewDecisionApprove) {
		t.Fatalf("submitted event = %v, want no approval",
			fixture.state.lastSubmitReview["event"])
	}
}

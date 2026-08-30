package review_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/review"
)

const (
	testHeadSHA           = "a3c4f1cac7f595bc824704b9d2a1f1191630dc32"
	testStaleHeadSHA      = "b4d5e2dbd8f606cd935815c0e3b2f2202741ed43"
	testBaseSHA           = "c5e6f3ece9f717de046926d1f4c3f3313852fe54"
	testPRNumber          = 7
	testMinimumImportance = 7
	testBotLogin          = "test-review-agent[bot]"
	testReviewModel       = "fixture-review-model"
)

// testClock advances a fixed amount on every read, so a rendered duration is
// deterministic without any sleeping. Chunks are timed from several goroutines
// at once, so the cursor is guarded; production reads time.Now, which is safe
// on its own.
func testClock(step time.Duration) func() time.Time {
	var mu sync.Mutex
	current := time.Unix(1_700_000_000, 0).UTC()
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now := current
		current = current.Add(step)
		return now
	}
}

func testSummary() review.Summary {
	return review.Summary{
		Head:              domain.HeadSHA(testHeadSHA),
		Decision:          domain.ReviewDecisionApprove,
		Models:            []string{testReviewModel},
		Duration:          8 * time.Second,
		FilesReviewed:     3,
		Chunks:            1,
		CoverageComplete:  true,
		MinimumImportance: 9,
	}
}

func TestDecisionForOnlyBlocksConfiguredFindings(t *testing.T) {
	t.Run("no findings", func(t *testing.T) {
		decision := review.DecisionFor(nil, 9)
		if decision != domain.ReviewDecisionApprove {
			t.Fatalf("decision = %q, want APPROVE", decision)
		}
	})

	t.Run("below configured level", func(t *testing.T) {
		findings := []domain.Finding{{
			Path:       "main.go",
			StartLine:  1,
			EndLine:    1,
			Title:      "Note",
			Body:       "Important but below this repository cutoff.",
			Importance: 8,
		}}
		decision := review.DecisionFor(findings, 9)
		if decision != domain.ReviewDecisionApprove {
			t.Fatalf("decision = %q, want APPROVE", decision)
		}
	})

	t.Run("at configured level", func(t *testing.T) {
		findings := []domain.Finding{{
			Path:       "main.go",
			StartLine:  1,
			EndLine:    1,
			Title:      "Blocker",
			Body:       "Must fix before merge.",
			Importance: 9,
		}}
		decision := review.DecisionFor(findings, 9)
		if decision != domain.ReviewDecisionRequestChanges {
			t.Fatalf("decision = %q, want %q", decision, domain.ReviewDecisionRequestChanges)
		}
	})
}

func TestRenderBodyLeadsWithTheVerdictThenTheDetails(t *testing.T) {
	head := domain.HeadSHA(testHeadSHA)
	tests := []struct {
		name     string
		decision domain.ReviewDecision
		message  string
	}{
		{name: "approve", decision: domain.ReviewDecisionApprove, message: "No severe findings."},
		{name: "request changes", decision: domain.ReviewDecisionRequestChanges, message: "Severe findings are listed inline."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary := testSummary()
			summary.Decision = test.decision
			body := review.RenderBody(summary)
			want := "## Review\n\n" + test.message + "\n\n" +
				review.RenderDetails(summary) + "\n\n" +
				marker.Summary() + "\n" + marker.Review(head)
			if body != want {
				t.Fatalf("body = %q, want %q", body, want)
			}
		})
	}
}

func TestRenderDetailsReportsEveryReviewStatistic(t *testing.T) {
	summary := testSummary()
	summary.Observed = []domain.Finding{{Importance: 8}, {Importance: 10}}
	summary.Eligible = []domain.Finding{{Importance: 10}}
	summary.Published = []domain.Finding{{Importance: 10}}
	summary.PriorReviews = nil
	summary.Threads = nil

	details := review.RenderDetails(summary)
	for _, want := range []string{
		"<details>",
		"<summary>Review details</summary>",
		"| Model | `" + testReviewModel + "` |",
		"| Duration | `8` seconds |",
		"| Head | `a3c4f1c` |",
		"| Files reviewed | `3` |",
		"| Diff chunks | `1` |",
		"| Coverage complete | yes |",
		"| Minimum importance | `9` |",
		"| Findings observed | `2` at importance `8`, `10` |",
		"| Findings eligible | `1` at importance `10` |",
		"| Findings published inline | `1` at importance `10` |",
		"| Prior bot review IDs | none |",
		"| Bot thread IDs | none |",
		"| Bot threads resolved | `0` |",
		"</details>",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
}

func TestRenderDetailsNamesEveryModelThatAnswered(t *testing.T) {
	summary := testSummary()
	summary.Models = []string{"gpt-5.6-sol", "gpt-5.4-nano"}

	details := review.RenderDetails(summary)
	if !strings.Contains(details, "| Model | `gpt-5.6-sol`, `gpt-5.4-nano` |") {
		t.Fatalf("details = %q, want both models", details)
	}
}

func TestRenderDetailsReportsAnUnknownModelWhenNoneAnswered(t *testing.T) {
	summary := testSummary()
	summary.Models = nil

	if !strings.Contains(review.RenderDetails(summary), "| Model | unknown |") {
		t.Fatalf("details = %q, want an unknown model", review.RenderDetails(summary))
	}
}

func TestRenderDetailsWritesOneSecondWithoutAPlural(t *testing.T) {
	summary := testSummary()
	summary.Duration = 1400 * time.Millisecond

	if !strings.Contains(review.RenderDetails(summary), "| Duration | `1` second |") {
		t.Fatalf("details = %q, want one second", review.RenderDetails(summary))
	}
}

// A finding marker inside the summary body would be read back by
// collectPublicationState and would silence that finding forever.
func TestRenderBodyCarriesTheRequiredMarkersAndNoFindingMarker(t *testing.T) {
	body := review.RenderBody(testSummary())

	if !marker.HasSummary(body) {
		t.Fatalf("body = %q, want the summary marker", body)
	}
	head, found := marker.FindReview(body)
	if !found || head != domain.HeadSHA(testHeadSHA) {
		t.Fatalf("body = %q, want the review marker for the head", body)
	}
	if _, found := marker.FindFinding(body); found {
		t.Fatalf("body = %q, want no finding marker", body)
	}
}

func TestRenderFailureBodyStatesTheFailureAndOmitsTheReviewMarker(t *testing.T) {
	t.Run("stated failure", func(t *testing.T) {
		summary := testSummary()
		detail := "The cause is recorded in this service's log."
		body := review.RenderFailureBody(summary, "Review failed.", detail)
		summary.Failed = true
		want := "## Review\n\nReview failed.\n\n" + detail + "\n\n" +
			review.RenderDetails(summary) + "\n\n" + marker.Summary()
		if body != want {
			t.Fatalf("body = %q, want %q", body, want)
		}
	})

	t.Run("no detail", func(t *testing.T) {
		summary := testSummary()
		body := review.RenderFailureBody(summary, "Review failed.", "   ")
		summary.Failed = true
		want := "## Review\n\nReview failed.\n\n" + review.RenderDetails(summary) + "\n\n" + marker.Summary()
		if body != want {
			t.Fatalf("body = %q, want %q", body, want)
		}
	})

	t.Run("never carries a review marker", func(t *testing.T) {
		body := review.RenderFailureBody(testSummary(), "Review failed.", "provider refused")
		if _, found := marker.FindReview(body); found {
			t.Fatalf("body = %q, want no review marker", body)
		}
	})
}

func TestRenderFailureBodyReportsWhatTheReviewLearned(t *testing.T) {
	summary := testSummary()
	summary.Reached = "the diff"
	summary.Models = []string{"gpt-5.6-sol"}
	summary.FilesReviewed = 4
	summary.Chunks = 6

	body := review.RenderFailureBody(summary, "Review failed during model analysis.", "provider refused")
	for _, want := range []string{
		"| Model | `gpt-5.6-sol` |",
		"| Duration | `8` seconds |",
		"| Head | `a3c4f1c` |",
		"| Files reviewed | `4` |",
		"| Diff chunks | `6` |",
		"| Reached | the diff |",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("failure body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Coverage complete") {
		t.Fatalf("failure body claims coverage it never established:\n%s", body)
	}
}

func TestRenderDetailsReportsNothingReachedWhenTheReviewFailedImmediately(t *testing.T) {
	summary := review.Summary{Failed: true}

	details := review.RenderDetails(summary)
	if !strings.Contains(details, "| Reached | nothing |") {
		t.Fatalf("details = %q, want a nothing reached row", details)
	}
	if !strings.Contains(details, "| Model | unknown |") {
		t.Fatalf("details = %q, want an unknown model", details)
	}
}

func TestRenderInlineUsesRightSideRangesAndFindingMarkers(t *testing.T) {
	head := domain.HeadSHA(testHeadSHA)
	findings := []domain.Finding{{
		Path:       "main.go",
		StartLine:  4,
		EndLine:    6,
		Title:      "Validate `rangeEnd`",
		Body:       "An unchecked `rangeEnd` can exceed the buffer and panic. Reject values above `len(buffer)` before slicing.",
		Suggestion: "if rangeEnd > len(buffer) {\n\treturn errRange\n}",
		Importance: 9,
	}}

	comments, err := review.RenderInline(head, findings)
	if err != nil {
		t.Fatalf("RenderInline: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("comment count = %d, want 1", len(comments))
	}

	comment := comments[0]
	if comment.Path != "main.go" {
		t.Fatalf("path = %q, want main.go", comment.Path)
	}
	if comment.Line != 6 || comment.Side != "RIGHT" {
		t.Fatalf("comment = %+v, want RIGHT side ending at line 6", comment)
	}
	if comment.StartLine != 4 || comment.StartSide != "RIGHT" {
		t.Fatalf("comment = %+v, want RIGHT multiline start at line 4", comment)
	}
	if _, ok := marker.FindFinding(comment.Body); !ok {
		t.Fatalf("comment body missing finding marker: %q", comment.Body)
	}
	if strings.Contains(comment.Body, "Importance:") {
		t.Fatalf("comment body exposes numeric importance: %q", comment.Body)
	}
	if !strings.Contains(comment.Body, "### Validate `rangeEnd`") {
		t.Fatalf("comment body missing inline code heading: %q", comment.Body)
	}
	wantSuggestion := "```suggestion\n" + findings[0].Suggestion + "\n```"
	if !strings.Contains(comment.Body, wantSuggestion) {
		t.Fatalf("comment body missing suggestion: %q", comment.Body)
	}
	findingMarker, err := marker.Finding(head, findings[0])
	if err != nil {
		t.Fatalf("Finding marker: %v", err)
	}
	if !strings.HasSuffix(comment.Body, findingMarker) {
		t.Fatalf("comment body does not end with finding marker: %q", comment.Body)
	}
}

func TestRenderedProseHasNoTypographicDashes(t *testing.T) {
	head := domain.HeadSHA(testHeadSHA)
	analysis := struct {
		Summary    string
		Anchored   []domain.Finding
		Unanchored []domain.Finding
	}{
		Summary: "Issue — details",
		Anchored: []domain.Finding{{
			Path:       "main.go",
			StartLine:  2,
			EndLine:    2,
			Title:      "Title – note",
			Body:       "Body — impact",
			Importance: 5,
		}},
		Unanchored: []domain.Finding{{
			Path:       "other.go",
			StartLine:  1,
			EndLine:    1,
			Title:      "Unanchored – title",
			Body:       "Unanchored — body",
			Importance: 3,
		}},
	}

	body := review.RenderBody(testSummary())
	if containsTypographicDash(body) {
		t.Fatalf("review body still contains typographic dash: %q", body)
	}

	comments, err := review.RenderInline(head, analysis.Anchored)
	if err != nil {
		t.Fatalf("RenderInline: %v", err)
	}
	for _, comment := range comments {
		if containsTypographicDash(comment.Body) {
			t.Fatalf("inline body still contains typographic dash: %q", comment.Body)
		}
	}
}

// truncatedModel truncates the first calls, then answers. It reproduces a model
// that reaches its completion budget on a chunk carrying too many hunks.
type truncatedModel struct {
	truncateCalls int
	prompts       []string
	calls         int
	findings      []domain.Finding
}

func (model *truncatedModel) Review(_ context.Context, prompt string) (review.Completion, error) {
	model.calls++
	model.prompts = append(model.prompts, prompt)
	if model.calls <= model.truncateCalls {
		return review.Completion{}, &openai.TruncatedError{Model: testReviewModel}
	}
	return review.Completion{
		Result: domain.ReviewResult{CoverageComplete: true, Findings: model.findings},
		Model:  testReviewModel,
	}, nil
}

// multiHunkCollector returns one file whose patch has four hunks, so its single
// chunk can split twice before reaching the one hunk floor.
type multiHunkCollector struct{}

func (multiHunkCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	patch := strings.Join([]string{
		"@@ -1,2 +1,3 @@",
		" package main",
		"+added1",
		"@@ -20,2 +21,3 @@",
		" func b() {}",
		"+added2",
		"@@ -40,2 +41,3 @@",
		" func c() {}",
		"+added3",
		"@@ -60,2 +61,3 @@",
		" func d() {}",
		"+added4",
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
			CurrentContent:    "package main\nadded1\nadded2\nadded3\nadded4\n",
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		}},
	}, nil
}

// A model that reaches its completion token budget stops mid answer. The chunk
// splits in half and each half is reviewed instead, inside the same per chunk
// call, so the finding the answer carried still reaches the pull request.
func TestATruncatedChunkSplitsAndRetriesInsideItsOwnCall(t *testing.T) {
	model := &truncatedModel{
		truncateCalls: 1,
		findings: []domain.Finding{{
			Path:       "main.go",
			StartLine:  2,
			EndLine:    2,
			Title:      "Defect",
			Body:       "The changed line breaks behavior.",
			Importance: 9,
		}},
	}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         multiHunkCollector{},
		minimumImportance: 9,
		model:             model,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if model.calls != 3 {
		t.Fatalf("model calls = %d, want 3: one truncated, then both halves", model.calls)
	}
	if len(fixture.state.streamedComments) != 1 {
		t.Fatalf("streamed comments = %d, want the finding recovered from the halves",
			len(fixture.state.streamedComments))
	}
	if state := decodedSummaryState(t, fixture); len(state.Pending) != 0 {
		t.Fatalf("pending = %v, want none: every hunk was reviewed", state.Pending)
	}
}

func TestTheSplitRecursesWhileTheAnswerKeepsTruncating(t *testing.T) {
	model := &truncatedModel{truncateCalls: 2}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         multiHunkCollector{},
		minimumImportance: 9,
		model:             model,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Four hunks: the whole chunk truncates, its first half truncates again,
	// then both single hunks answer, then the second half answers.
	if model.calls != 5 {
		t.Fatalf("model calls = %d, want 5 as the split recurses twice", model.calls)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("event = %v, want APPROVE because every hunk was eventually reviewed",
			fixture.state.lastSubmitReview["event"])
	}
}

// A hunk that cannot split is skipped rather than failing the chunk, so the
// head is not fully reviewed and nothing here may approve it.
func TestAHunkThatCannotSplitLeavesTheHeadUnapproved(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         multiHunkCollector{},
		minimumImportance: 9,
		model:             &truncatedModel{truncateCalls: 1000},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v, want REQUEST_CHANGES over a head that was not fully read",
			fixture.state.lastSubmitReview["event"])
	}
	body := failureSummaryComment(t, fixture)
	if !strings.Contains(body, "This head was not fully reviewed") {
		t.Fatalf("summary comment does not say the head went partly unread:\n%s", body)
	}
	if !strings.Contains(body, "| Coverage complete | no |") {
		t.Fatalf("summary comment claims coverage it never established:\n%s", body)
	}
	if len(fixture.state.streamedComments) != 0 {
		t.Fatalf("streamed comments = %d, want none", len(fixture.state.streamedComments))
	}
}

// A refusal is not truncation, so nothing splits and nothing retries. The chunk
// stays pending for the next push and the run does not fail.
func TestAModelRefusalIsNotRetriedAndLeavesItsChunkPending(t *testing.T) {
	model := &sequenceModel{err: errors.New("provider refused the prompt")}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector: multiHunkCollector{},
		model:     model,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(model.prompts) != 1 {
		t.Fatalf("model calls = %d, want 1 because splitting cannot fix a refusal", len(model.prompts))
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "neutral" {
		t.Fatalf("conclusion = %v, want neutral", fixture.state.lastUpdateCheckRun["conclusion"])
	}
	if state := decodedSummaryState(t, fixture); len(state.Pending) != 1 {
		t.Fatalf("pending = %v, want the refused chunk", state.Pending)
	}
	assertBlockingVerdictOverAnUnreadHead(t, fixture)
}

// assertBlockingVerdictOverAnUnreadHead proves a run that could not read the
// whole head blocks it and says why, and that the verdict it submits carries no
// review marker.
//
// Withholding the verdict is what leaves the previous run's approval standing
// over a head nobody finished reading, and a review marker here would tell the
// next run this head was already reviewed and suppress the run that finishes it.
func assertBlockingVerdictOverAnUnreadHead(t *testing.T, fixture *serviceFixture) {
	t.Helper()
	if fixture.state.lastSubmitReview == nil {
		t.Fatal("no verdict was submitted, want a blocking one over a head that was not fully read")
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v, want REQUEST_CHANGES", fixture.state.lastSubmitReview["event"])
	}
	body, ok := fixture.state.lastSubmitReview["body"].(string)
	if !ok {
		t.Fatalf("verdict body = %v, want a string", fixture.state.lastSubmitReview["body"])
	}
	if !strings.Contains(body, "could not be reviewed") {
		t.Fatalf("verdict body does not say what is holding it:\n%s", body)
	}
	if _, found := marker.FindReview(body); found {
		t.Fatalf("verdict body carries a review marker, which suppresses the run that finishes the head:\n%s", body)
	}
	if marker.HasSummary(body) {
		t.Fatalf("verdict body carries the summary marker, which makes it a second visible summary:\n%s", body)
	}
}

// twoChunkSameFileCollector returns one file whose two hunks are each large
// enough to need their own chunk, so both chunks describe the same file and a
// finding reported by both is the same defect.
type twoChunkSameFileCollector struct{}

func (twoChunkSameFileCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	patch := strings.Join([]string{
		"@@ -1,2 +1,3 @@",
		" package main",
		"+added1",
		"@@ -100,2 +101,3 @@",
		" func other() {}",
		"+added2",
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
			CurrentContent:    strings.Repeat("x\n", 20000),
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		}},
	}, nil
}

// One defect reported by two chunks is one comment, and a finding that does not
// anchor to a changed line is never published at all.
func TestTheSameFindingFromTwoChunksIsPublishedOnce(t *testing.T) {
	duplicate := domain.Finding{
		Path:       "main.go",
		StartLine:  2,
		EndLine:    2,
		Title:      "Duplicate",
		Body:       "Same finding.",
		Importance: 9,
	}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         twoChunkSameFileCollector{},
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{
			{CoverageComplete: true, Findings: []domain.Finding{duplicate}},
			{CoverageComplete: true, Findings: []domain.Finding{
				duplicate,
				{
					Path:       "main.go",
					StartLine:  99,
					EndLine:    99,
					Title:      "Bad anchor",
					Body:       "Line is outside the diff.",
					Importance: 10,
				},
				{
					Path:       "../escape.go",
					StartLine:  1,
					EndLine:    1,
					Title:      "Bad path",
					Body:       "Path normalizes to traversal.",
					Importance: 10,
				},
			}},
		}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.model.(*sequenceModel).callCount != 2 {
		t.Fatalf("model call count = %d, want one call per chunk", fixture.model.(*sequenceModel).callCount)
	}
	if len(fixture.state.streamedComments) != 1 {
		t.Fatalf("streamed comments = %d, want the one anchored defect", len(fixture.state.streamedComments))
	}
	body, ok := fixture.state.streamedComments[0]["body"].(string)
	if !ok || !strings.Contains(body, "Duplicate") {
		t.Fatalf("comment body = %v, want the anchored duplicate", fixture.state.streamedComments[0]["body"])
	}
}

func TestTheChunkPromptClassifiesFindingsAndWrapsUntrustedInput(t *testing.T) {
	model := &sequenceModel{results: []domain.ReviewResult{{CoverageComplete: true}}}
	fixture := newServiceFixture(t, serviceFixtureOptions{minimumImportance: 9, model: model})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(model.prompts) != 1 {
		t.Fatalf("prompt count = %d, want 1", len(model.prompts))
	}
	prompt := model.prompts[0]
	for _, want := range []string{
		"importance 9 or higher",
		"backticks",
		"exact replacement",
		"<<<UNTRUSTED_INPUT>>>",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %q", want, prompt)
		}
	}
}

// A model answer the schema rejects is a failed chunk like any other: it stays
// pending, and the run blocks the head rather than approving what it never read.
func TestAnInvalidModelResultLeavesItsChunkPending(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &sequenceModel{results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings: []domain.Finding{{
				Path:       "main.go",
				StartLine:  2,
				EndLine:    2,
				Title:      "",
				Body:       "Invalid title.",
				Importance: 5,
			}},
		}}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state := decodedSummaryState(t, fixture); len(state.Pending) != 1 {
		t.Fatalf("pending = %v, want the chunk whose answer was rejected", state.Pending)
	}
	assertBlockingVerdictOverAnUnreadHead(t, fixture)
}

// sequenceModel answers a fixed sequence of results, one per chunk, in the
// order the chunk loop asks for them.
type sequenceModel struct {
	mu        sync.Mutex
	results   []domain.ReviewResult
	models    []string
	prompts   []string
	callCount int
	err       error
}

func (model *sequenceModel) Review(_ context.Context, prompt string) (review.Completion, error) {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.prompts = append(model.prompts, prompt)
	if model.err != nil {
		return review.Completion{}, model.err
	}
	if model.callCount >= len(model.results) {
		return review.Completion{}, errors.New("unexpected model call")
	}
	result := model.results[model.callCount]
	name := testReviewModel
	if model.callCount < len(model.models) {
		name = model.models[model.callCount]
	}
	model.callCount++
	return review.Completion{Result: result, Model: name}, nil
}

type contextBlockingModel struct{}

func (contextBlockingModel) Review(ctx context.Context, _ string) (review.Completion, error) {
	<-ctx.Done()
	return review.Completion{}, ctx.Err()
}

type panicModel struct{}

func (panicModel) Review(context.Context, string) (review.Completion, error) {
	panic("model panic")
}

// assertCheckAndCommentShareDetails proves the comment and the check run render
// from one source, so the two can never report different numbers.
func assertCheckAndCommentShareDetails(t *testing.T, fixture *serviceFixture, body string) {
	t.Helper()
	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("check output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	details, ok := output["summary"].(string)
	if !ok || !strings.HasPrefix(details, "<details>") {
		t.Fatalf("check summary = %v, want the rendered detail block", output["summary"])
	}
	if !strings.Contains(body, details) {
		t.Fatalf("comment body does not carry the check run detail block\nbody:\n%s\ncheck:\n%s", body, details)
	}
}

func containsTypographicDash(value string) bool {
	for _, character := range value {
		switch character {
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
			return true
		}
	}
	return false
}

// Every chunk failing is still not a failed run. Nothing was read, so nothing
// is claimed: every chunk stays pending, the head is blocked rather than
// approved, and the next push reviews the whole delta again.
func TestEveryChunkFailingLeavesEveryChunkPendingAndBlocksTheHead(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector: twoChunkCollector{},
		model:     &sequenceModel{err: errors.New("provider refused the prompt")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	state := decodedSummaryState(t, fixture)
	if len(state.Pending) != 2 {
		t.Fatalf("pending = %v, want both chunks", state.Pending)
	}
	if state.LastReviewed != "" {
		t.Fatalf("last reviewed = %q, want no checkpoint at all", state.LastReviewed)
	}
	assertBlockingVerdictOverAnUnreadHead(t, fixture)
	if fixture.state.lastUpdateCheckRun["conclusion"] != "neutral" {
		t.Fatalf("conclusion = %v, want neutral", fixture.state.lastUpdateCheckRun["conclusion"])
	}
}

// A chunk that answers still publishes what it found, even while another chunk
// of the same delta goes unread. The unread one stays pending; the finding does
// not wait for it.
func TestAnAnsweringChunkPublishesWhileAnotherStaysPending(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         twoChunkCollector{},
		minimumImportance: 9,
		model:             newChunkScriptedModel("file1.go"),
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fixture.state.streamedComments) != 1 {
		t.Fatalf("streamed comments = %d, want the answering chunk's finding",
			len(fixture.state.streamedComments))
	}
	if state := decodedSummaryState(t, fixture); len(state.Pending) != 1 {
		t.Fatalf("pending = %v, want only the chunk that went unread", state.Pending)
	}
}

// A reader who opens a check run must see what the review did, not only the
// stage it stopped at. Before this, "Review failed during model analysis." was
// the whole explanation and the per-chunk detail existed nowhere readable.
func TestARunThatLeavesChunksPendingPublishesItsOwnLogInTheCheckRun(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model:             &sequenceModel{err: errors.New("model provider request timed out")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fixture.state.lastUpdateCheckRun["conclusion"] != "neutral" {
		t.Fatalf("conclusion = %v, want neutral", fixture.state.lastUpdateCheckRun["conclusion"])
	}
	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	text, ok := output["text"].(string)
	if !ok {
		t.Fatalf("output text = %v, want the run log", output["text"])
	}
	for _, want := range []string{
		"## Run log",
		"review job started",
		"review model analysis started",
		"delivery_id=delivery-1",
		// The line that carried the cause is still there, and still says it
		// carried one, so a reader can see where the run went wrong.
		"chunk review failed, leaving it pending",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("run log missing %q:\n%s", want, text)
		}
	}
	// The published log is on a check run, which is as public and as permanent
	// as a comment, so the provider's own sentence is not in it.
	if strings.Contains(text, "model provider request timed out") {
		t.Fatalf("published run log carries the raw provider cause:\n%s", text)
	}
	if !strings.Contains(text, "err=[redacted: see this service's log for this run]") {
		t.Fatalf("published run log does not say a cause was withheld:\n%s", text)
	}
}

// PR 282 failed with "review chunk 12/13: model provider request timed out" and
// nothing said how the 570 second budget was spent. The published log must name
// each chunk's duration so a slow run and a hung call look different.
func TestPublishedRunLogNamesEachChunkDuration(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model: &sequenceModel{
			results: []domain.ReviewResult{{CoverageComplete: true, Findings: nil}},
		},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	text, ok := output["text"].(string)
	if !ok {
		t.Fatalf("output text = %v, want the run log", output["text"])
	}
	for _, want := range []string{
		"review chunk completed",
		"chunk=1",
		"chunks=1",
		"elapsed=",
		"model=" + testReviewModel,
		"prompt_bytes=",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("run log missing %q:\n%s", want, text)
		}
	}
}

// A truncated request still spends its duration on the review budget. The split
// and skip lines that follow truncation carry neither the duration nor the
// prompt size, so without this line the request that caused the split is
// untimed and a reader cannot tell a slow truncation from a fast one.
func TestPublishedRunLogTimesATruncatedRequest(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model:             &truncatedModel{truncateCalls: 1},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	text, ok := output["text"].(string)
	if !ok {
		t.Fatalf("output text = %v, want the run log", output["text"])
	}
	for _, want := range []string{
		"review chunk request failed",
		"truncated=true",
		"elapsed=",
		"prompt_bytes=",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("run log missing %q:\n%s", want, text)
		}
	}
}

// A successful review publishes its log too, so a slow run that still passed
// can be read the same way as one that failed.
func TestSuccessfulReviewPublishesItsOwnLogInTheCheckRun(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model: &sequenceModel{
			results: []domain.ReviewResult{{CoverageComplete: true, Findings: nil}},
		},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	text, ok := output["text"].(string)
	if !ok || !strings.Contains(text, "review model analysis completed") {
		t.Fatalf("output text = %v, want the completed analysis line", output["text"])
	}
}

// decodedSummaryState reads the durable state back from the one top level
// comment, which is where a later invocation reads it from too.
func decodedSummaryState(t *testing.T, fixture *serviceFixture) marker.State {
	t.Helper()
	state, ok := marker.DecodeState(failureSummaryComment(t, fixture))
	if !ok {
		t.Fatalf("summary comment carries no decodable state marker:\n%s", failureSummaryComment(t, fixture))
	}
	return state
}

// wideCollector returns one file with twelve changed lines, so one chunk can
// carry twelve separately anchored findings.
type wideCollector struct{}

func (wideCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	patchLines := []string{"@@ -1,1 +1,13 @@", " package main"}
	contentLines := []string{"package main"}
	for index := range 12 {
		patchLines = append(patchLines, fmt.Sprintf("+added%d", index))
		contentLines = append(contentLines, fmt.Sprintf("added%d", index))
	}
	patch := strings.Join(patchLines, "\n")
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
			CurrentContent:    strings.Join(contentLines, "\n") + "\n",
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		}},
	}, nil
}

// A reviewer does not ration comments. Every finding that anchors to a changed
// line and meets the importance floor is published, however many there are.
// The old cap of three silently hid the rest.
func TestEveryQualifyingFindingIsPublished(t *testing.T) {
	findings := make([]domain.Finding, 0, 12)
	for index := range 12 {
		findings = append(findings, domain.Finding{
			Path:       "main.go",
			StartLine:  index + 2,
			EndLine:    index + 2,
			Title:      fmt.Sprintf("Defect %d", index),
			Body:       "A real defect on a changed line.",
			Importance: 9,
		})
	}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         wideCollector{},
		minimumImportance: 9,
		model: &sequenceModel{
			results: []domain.ReviewResult{{CoverageComplete: true, Findings: findings}},
		},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fixture.state.streamedComments) != 12 {
		t.Fatalf("streamed comments = %d, want all 12: a reviewer does not ration comments",
			len(fixture.state.streamedComments))
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v, want REQUEST_CHANGES over the threads this run opened",
			fixture.state.lastSubmitReview["event"])
	}
}

// severeFinding is the one anchored defect most of these fixtures report.
func severeFinding() domain.Finding {
	return domain.Finding{
		Path:       "main.go",
		StartLine:  2,
		EndLine:    2,
		Title:      "Severe defect",
		Body:       "The changed line breaks core behavior.",
		Importance: 9,
	}
}

// The checkpoint is what a later invocation resumes from, so it may only
// advance over findings the reader can actually see. A post whose answer never
// arrived leaves its whole chunk pending, and the run neither fails nor
// approves.
func TestTheMarkerAdvancesOnlyAfterAChunksFindingsPost(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance:   9,
		createCommentHangup: true,
		model: &sequenceModel{results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings:         []domain.Finding{severeFinding()},
		}}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	state := decodedSummaryState(t, fixture)
	if len(state.Pending) != 1 {
		t.Fatalf("pending = %v, want the chunk whose comment never reached the page", state.Pending)
	}
	if !chunkIDShaped(state.Pending[0]) {
		t.Fatalf("pending id = %q, want twelve lowercase hex characters", state.Pending[0])
	}
	if state.LastReviewed != "" {
		t.Fatalf("last reviewed = %q, want no checkpoint over an unseen finding", state.LastReviewed)
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "neutral" {
		t.Fatalf("conclusion = %v, want neutral rather than a failure",
			fixture.state.lastUpdateCheckRun["conclusion"])
	}
	// The defect is real whether or not its comment reached the page, so the
	// run must never approve over it.
	assertBlockingVerdictOverAnUnreadHead(t, fixture)
}

// A comment GitHub answered and refused is not a transient failure. Retrying it
// on every later run would pin the pull request forever on an attempt already
// known to fail, so the chunk finishes and the checkpoint advances. The run
// still refuses to approve, because a finding nobody can see is still a finding.
func TestACommentGitHubRefusesFinishesItsChunkAndStillBlocks(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance:   9,
		createCommentStatus: http.StatusUnprocessableEntity,
		model: &sequenceModel{results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings:         []domain.Finding{severeFinding()},
		}}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	state := decodedSummaryState(t, fixture)
	if len(state.Pending) != 0 {
		t.Fatalf("pending = %v, want none: no later attempt can post a comment GitHub refused", state.Pending)
	}
	if state.LastReviewed != domain.HeadSHA(testHeadSHA) {
		t.Fatalf("last reviewed = %q, want the head: the chunk was read", state.LastReviewed)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v, want REQUEST_CHANGES despite the refused comment",
			fixture.state.lastSubmitReview["event"])
	}
	body, ok := fixture.state.issueComments[0]["body"].(string)
	if !ok || !strings.Contains(body, "This head was not fully reviewed") {
		t.Fatalf("summary comment does not say the head went partly unseen:\n%v",
			fixture.state.issueComments[0]["body"])
	}
}

// chunkIDShaped reports whether an id matches what the durable marker accepts.
func chunkIDShaped(id string) bool {
	if len(id) != 12 {
		return false
	}
	for _, character := range id {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// chunkScriptedModel refuses the chunk covering one path until it is healed,
// and answers every other chunk with a finding on that chunk's own file. It
// records which paths it was asked about, so a test can prove a chunk reached
// the model rather than only that a run finished.
type chunkScriptedModel struct {
	mu       sync.Mutex
	failPath string
	healthy  bool
	seen     []string
}

func newChunkScriptedModel(failPath string) *chunkScriptedModel {
	return &chunkScriptedModel{failPath: failPath}
}

func (model *chunkScriptedModel) heal() {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.healthy = true
}

func (model *chunkScriptedModel) reviewedPaths() []string {
	model.mu.Lock()
	defer model.mu.Unlock()
	return append([]string{}, model.seen...)
}

func (model *chunkScriptedModel) reviewed(path string) bool {
	for _, seen := range model.reviewedPaths() {
		if seen == path {
			return true
		}
	}
	return false
}

func (model *chunkScriptedModel) Review(_ context.Context, prompt string) (review.Completion, error) {
	path := "file0.go"
	for _, candidate := range []string{"file1.go", "file2.go"} {
		if strings.Contains(prompt, "File: "+candidate) {
			path = candidate
		}
	}

	model.mu.Lock()
	healthy := model.healthy
	failPath := model.failPath
	model.seen = append(model.seen, path)
	model.mu.Unlock()

	if !healthy && path == failPath {
		return review.Completion{}, errors.New("provider refused the prompt")
	}
	return review.Completion{
		Result: domain.ReviewResult{
			CoverageComplete: true,
			Findings: []domain.Finding{{
				Path:       path,
				StartLine:  2,
				EndLine:    2,
				Title:      "Defect in " + path,
				Body:       "The changed line breaks core behavior.",
				Importance: 9,
			}},
		},
		Model: testReviewModel,
	}, nil
}

// A chunk whose model call fails stays pending and visible, and the next run
// reviews it and nothing else. The chunk that already answered is not asked
// again, so its finding is not posted twice.
func TestAFailedChunkStaysPendingAndTheNextRunFinishesIt(t *testing.T) {
	model := newChunkScriptedModel("file1.go")
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         twoChunkCollector{},
		minimumImportance: 9,
		model:             model,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	first := decodedSummaryState(t, fixture)
	if len(first.Pending) != 1 {
		t.Fatalf("pending after the first run = %v, want the refused chunk", first.Pending)
	}
	if first.Status != marker.StateReviewing {
		t.Fatalf("status = %q, want %q", first.Status, marker.StateReviewing)
	}
	assertBlockingVerdictOverAnUnreadHead(t, fixture)
	if fixture.state.lastUpdateCheckRun["conclusion"] != "neutral" {
		t.Fatalf("conclusion = %v, want neutral", fixture.state.lastUpdateCheckRun["conclusion"])
	}
	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok || !strings.Contains(fmt.Sprint(output["title"]), "1 chunk could not be reviewed") {
		t.Fatalf("check title = %v, want the count of unread chunks", fixture.state.lastUpdateCheckRun["output"])
	}

	model.heal()
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	second := decodedSummaryState(t, fixture)
	if len(second.Pending) != 0 {
		t.Fatalf("pending after the second run = %v, want none", second.Pending)
	}
	if second.LastReviewed != domain.HeadSHA(testHeadSHA) {
		t.Fatalf("last reviewed = %q, want the head now that every chunk answered", second.LastReviewed)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v, want a verdict from the run that finished the head",
			fixture.state.lastSubmitReview["event"])
	}
	// The second run re-reads the chunk that already answered, because a delta
	// may have grown since. The finding it reports again is suppressed by the
	// thread the first run opened, so it is not posted twice.
	if len(fixture.state.streamedComments) != 2 {
		t.Fatalf("streamed comments = %d, want one per chunk and no repeat",
			len(fixture.state.streamedComments))
	}
	bodies := bodiesOf(fixture.state.streamedComments)
	if bodies[0] == bodies[1] {
		t.Fatal("the same comment was posted twice")
	}
}

// growingCollector returns one more chunk on its second call, which is what a
// push adds to a delta whose baseline has not moved.
type growingCollector struct {
	mu    sync.Mutex
	calls int
}

func (collector *growingCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	collector.mu.Lock()
	collector.calls++
	fileCount := 2
	if collector.calls > 1 {
		fileCount = 3
	}
	collector.mu.Unlock()
	return paddedFiles(pullRequest, fileCount)
}

// paddedFiles builds one file per chunk, each padded past the prompt size so
// the chunker cannot merge two of them.
func paddedFiles(pullRequest githubapp.PullRequest, count int) (diff.ReviewInput, error) {
	padding := strings.Repeat("x\n", 30000)
	files := make([]diff.FileContext, 0, count)
	for index := range count {
		patch := fmt.Sprintf("@@ -1,1 +1,2 @@\n line%d\n+added%d\n", index, index)
		changed, hunks, err := diff.ChangedRightLines(patch)
		if err != nil {
			return diff.ReviewInput{}, err
		}
		files = append(files, diff.FileContext{
			Path:              fmt.Sprintf("file%d.go", index),
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    padding,
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		})
	}
	return diff.ReviewInput{PullRequest: pullRequest, Files: files}, nil
}

// A run that left a chunk pending did not advance the baseline, so the next
// delta covers the old range plus everything pushed since. Reviewing only the
// pending ids would skip those new commits and then mark the whole range
// reviewed, which is unreviewed code merging while the service says it read it.
func TestTheNextRunReviewsNewChunksAndNotOnlyThePendingOnes(t *testing.T) {
	model := newChunkScriptedModel("file1.go")
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         &growingCollector{},
		minimumImportance: 9,
		model:             model,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if state := decodedSummaryState(t, fixture); len(state.Pending) != 1 {
		t.Fatalf("pending after the first run = %v, want the refused chunk", state.Pending)
	}

	// The second run's delta carries a third chunk the first run never saw.
	model.heal()
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if !model.reviewed("file2.go") {
		t.Fatalf("the new chunk was never sent to the model; paths reviewed = %v", model.reviewedPaths())
	}
	state := decodedSummaryState(t, fixture)
	if len(state.Pending) != 0 {
		t.Fatalf("pending = %v, want none", state.Pending)
	}
	if state.LastReviewed != domain.HeadSHA(testHeadSHA) {
		t.Fatalf("last reviewed = %q, want the head only once every chunk of the delta was read",
			state.LastReviewed)
	}
	// Closing the gap must not cost a re-analysis of what the first run read.
	if timesReviewed(model, "file0.go") != 1 {
		t.Fatalf("file0.go was analyzed %d times, want once: it answered on the first run",
			timesReviewed(model, "file0.go"))
	}
}

func timesReviewed(model *chunkScriptedModel, path string) int {
	count := 0
	for _, seen := range model.reviewedPaths() {
		if seen == path {
			count++
		}
	}
	return count
}

// A chunk that answered is recorded as done, so the next run subtracts it from
// the delta instead of paying for it again. Without that record, one failed
// chunk in a sixty chunk delta costs sixty model calls on the next run to
// deliver one.
func TestAnAlreadyReadChunkIsNotAnalyzedTwice(t *testing.T) {
	model := newChunkScriptedModel("file1.go")
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         twoChunkCollector{},
		minimumImportance: 9,
		model:             model,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	first := decodedSummaryState(t, fixture)
	if len(first.Completed) != 1 {
		t.Fatalf("completed after the first run = %v, want the chunk that answered", first.Completed)
	}
	if !chunkIDShaped(first.Completed[0]) {
		t.Fatalf("completed id = %q, want twelve lowercase hex characters", first.Completed[0])
	}

	model.heal()
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if timesReviewed(model, "file0.go") != 1 {
		t.Fatalf("file0.go was analyzed %d times, want once", timesReviewed(model, "file0.go"))
	}
	if timesReviewed(model, "file1.go") != 2 {
		t.Fatalf("file1.go was analyzed %d times, want twice: it failed and was retried by the next run",
			timesReviewed(model, "file1.go"))
	}
	// The record is meaningless once the baseline moves, so it is dropped
	// rather than left to grow across the life of the pull request.
	second := decodedSummaryState(t, fixture)
	if len(second.Completed) != 0 {
		t.Fatalf("completed after the baseline advanced = %v, want none", second.Completed)
	}
	if second.LastReviewed != domain.HeadSHA(testHeadSHA) {
		t.Fatalf("last reviewed = %q, want the head", second.LastReviewed)
	}
}

// Two incomplete runs must leave one blocking review, not one each. A reader
// facing a stack of short verdicts cannot tell which still describes the pull
// request.
func TestTwoIncompleteRunsLeaveOneBlockingReview(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         twoChunkCollector{},
		minimumImportance: 9,
		model:             newChunkScriptedModel("file1.go"),
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(fixture.state.submittedReviews) != 1 {
		t.Fatalf("submitted reviews after the first run = %d, want 1", len(fixture.state.submittedReviews))
	}
	if fixture.state.lastUpdateReview != nil {
		t.Fatalf("updated review = %v, want none on the first run", fixture.state.lastUpdateReview)
	}

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if len(fixture.state.submittedReviews) != 1 {
		t.Fatalf("submitted reviews = %d, want the first one rewritten rather than a second left beside it",
			len(fixture.state.submittedReviews))
	}
	if fixture.state.lastUpdateReview == nil {
		t.Fatal("the second run submitted a new review instead of rewriting its own standing verdict")
	}
	body, ok := fixture.state.lastUpdateReview["body"].(string)
	if !ok || !marker.HasPartial(body) {
		t.Fatalf("rewritten body = %v, want the partial verdict marker", fixture.state.lastUpdateReview["body"])
	}
	// An update changes a body and not a state, so the standing request for
	// changes is still standing.
	if !strings.Contains(body, "could not be reviewed") {
		t.Fatalf("rewritten body does not say what is holding it:\n%s", body)
	}
}

func bodiesOf(comments []map[string]any) []string {
	bodies := make([]string, 0, len(comments))
	for _, comment := range comments {
		body, _ := comment["body"].(string)
		bodies = append(bodies, body)
	}
	return bodies
}

// manyChunkCollector returns one more file than the concurrency limit, so the
// last chunk can only start after an earlier one has finished.
type manyChunkCollector struct{}

func (manyChunkCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	return paddedFiles(pullRequest, config.MaximumChunkConcurrency+1)
}

// deadlineProbeModel measures each call's budget from that call's own start,
// and burns real time in the first wave so a later wave starts visibly later.
type deadlineProbeModel struct {
	mu        sync.Mutex
	firstWait time.Duration
	waves     int
	budgets   []time.Duration
	undated   int
}

func newDeadlineProbeModel(firstWait time.Duration) *deadlineProbeModel {
	return &deadlineProbeModel{firstWait: firstWait}
}

func (model *deadlineProbeModel) Review(ctx context.Context, _ string) (review.Completion, error) {
	started := time.Now()
	deadline, dated := ctx.Deadline()

	model.mu.Lock()
	if !dated {
		model.undated++
	}
	model.budgets = append(model.budgets, deadline.Sub(started))
	model.waves++
	inFirstWave := model.waves <= config.MaximumChunkConcurrency
	model.mu.Unlock()

	if inFirstWave {
		time.Sleep(model.firstWait)
	}
	return review.Completion{
		Result: domain.ReviewResult{CoverageComplete: true, Findings: nil},
		Model:  testReviewModel,
	}, nil
}

// shortestBudget is the least time any one call was given, which is where a
// clock shared across calls shows up.
func (model *deadlineProbeModel) shortestBudget() (time.Duration, int, int) {
	model.mu.Lock()
	defer model.mu.Unlock()
	shortest := time.Duration(0)
	for index, budget := range model.budgets {
		if index == 0 || budget < shortest {
			shortest = budget
		}
	}
	return shortest, len(model.budgets), model.undated
}

// No clock spans two model calls. Every chunk builds its own timeout when its
// call starts, so a chunk that answers slowly takes nothing from the chunks
// after it. One shared deadline is what made a thirteen chunk review die on
// chunk twelve with the first eleven chunks' work thrown away.
//
// Concurrency is why this measures each call's own budget rather than comparing
// two deadlines: calls in one wave start together and would share a deadline
// either way. The wave after the first is the one that tells them apart.
func TestNoModelCallInheritsAnEarlierChunksClock(t *testing.T) {
	const (
		firstWaveWork = 600 * time.Millisecond
		chunkTimeout  = 5 * time.Second
	)
	model := newDeadlineProbeModel(firstWaveWork)
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:    manyChunkCollector{},
		model:        model,
		chunkTimeout: chunkTimeout,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	shortest, calls, undated := model.shortestBudget()
	if undated != 0 {
		t.Fatalf("%d model calls carried no deadline, want every call bounded", undated)
	}
	if calls != config.MaximumChunkConcurrency+1 {
		t.Fatalf("model calls = %d, want one per chunk", calls)
	}
	// One clock over the whole pass would leave the last wave short by the time
	// the first wave spent. A clock per call leaves every call the full budget.
	if shortest < chunkTimeout-firstWaveWork/2 {
		t.Fatalf("shortest call budget = %s, want close to %s: a later call inherited an earlier clock",
			shortest, chunkTimeout)
	}
}

// concurrencyProbeModel records how many calls overlap, so a test can prove
// chunks run together rather than one after another.
type concurrencyProbeModel struct {
	mu       sync.Mutex
	inFlight int
	highest  int
	release  chan struct{}
}

func newConcurrencyProbeModel() *concurrencyProbeModel {
	return &concurrencyProbeModel{release: make(chan struct{})}
}

func (model *concurrencyProbeModel) Review(context.Context, string) (review.Completion, error) {
	model.mu.Lock()
	model.inFlight++
	if model.inFlight > model.highest {
		model.highest = model.inFlight
	}
	reached := model.inFlight >= config.MaximumChunkConcurrency
	model.mu.Unlock()

	// The first full wave releases everyone, so the test proves overlap without
	// depending on wall clock timing.
	if reached {
		model.releaseOnce()
	}
	<-model.release

	model.mu.Lock()
	model.inFlight--
	model.mu.Unlock()
	return review.Completion{
		Result: domain.ReviewResult{CoverageComplete: true, Findings: nil},
		Model:  testReviewModel,
	}, nil
}

func (model *concurrencyProbeModel) releaseOnce() {
	select {
	case <-model.release:
	default:
		close(model.release)
	}
}

func (model *concurrencyProbeModel) peak() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.highest
}

// Chunks are reviewed several at a time, under a bound. Strictly one at a time
// would take hours on a sixty chunk delta at the measured call durations, and
// unbounded would put that whole delta on the provider at once.
func TestChunksAreReviewedConcurrentlyUnderABound(t *testing.T) {
	model := newConcurrencyProbeModel()
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector: manyChunkCollector{},
		model:     model,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if model.peak() < 2 {
		t.Fatalf("peak concurrent model calls = %d, want more than one chunk in flight", model.peak())
	}
	if model.peak() > config.MaximumChunkConcurrency {
		t.Fatalf("peak concurrent model calls = %d, want at most %d",
			model.peak(), config.MaximumChunkConcurrency)
	}
}

func TestEndToEndApprovesBelowConfiguredImportance(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model: &sequenceModel{
			results: []domain.ReviewResult{{
				CoverageComplete: true,
				Findings: []domain.Finding{{
					Path:       "main.go",
					StartLine:  2,
					EndLine:    2,
					Title:      "Style note",
					Body:       "Consider renaming for clarity.",
					Importance: 8,
				}},
			}},
		},
	})

	err := fixture.run(context.Background(), fixture.job())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("event = %v, want APPROVE", fixture.state.lastSubmitReview["event"])
	}
	body, ok := fixture.state.lastSubmitReview["body"].(string)
	if !ok {
		t.Fatalf("body = %v, want string", fixture.state.lastSubmitReview["body"])
	}
	if !strings.HasPrefix(body, "## Review\n\nNo severe findings.\n\n") {
		t.Fatalf("body = %q, want the verdict first", body)
	}
	if !strings.Contains(body, "| Model | `"+testReviewModel+"` |") {
		t.Fatalf("body = %q, want the model that answered", body)
	}
	assertCheckAndCommentShareDetails(t, fixture, body)
	comments, ok := fixture.state.lastSubmitReview["comments"].([]any)
	if !ok || len(comments) != 0 {
		t.Fatalf("comments = %v, want none", fixture.state.lastSubmitReview["comments"])
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "success" {
		t.Fatalf("conclusion = %v, want success", fixture.state.lastUpdateCheckRun["conclusion"])
	}
	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	if output["title"] != "Approved" {
		t.Fatalf("title = %v, want Approved", output["title"])
	}
	summary, ok := output["summary"].(string)
	if !ok {
		t.Fatalf("summary = %v, want string", output["summary"])
	}
	for _, want := range []string{
		"| Minimum importance | `9` |",
		"| Findings observed | `1` at importance `8` |",
		"| Findings eligible | `0` |",
		"| Findings published inline | `0` |",
		"| Prior bot review IDs | none |",
		"| Bot thread IDs | none |",
		"| Files reviewed | `1` |",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
	if fixture.state.checkDetailsURL != "https://github.com/owner/repo" {
		t.Fatalf("details URL = %q, want repository URL", fixture.state.checkDetailsURL)
	}
}

func TestServicePublishesOneCompleteReviewAndCompletesCheck(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})
	fixture.state.reviewPages = [][]map[string]any{{}}

	err := fixture.run(context.Background(), fixture.job())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.reconciler.callCount != 1 {
		t.Fatalf("reconcile call count = %d, want 1", fixture.reconciler.callCount)
	}

	// The finding posts as its chunk answers, the checkpoint follows it, and
	// only then come the head refresh, the thread read the verdict is computed
	// from, and the review that carries it.
	wantOrder := []string{
		"GET /repos/owner/repo/commits/a3c4f1cac7f595bc824704b9d2a1f1191630dc32/check-runs",
		"POST /repos/owner/repo/check-runs",
		"PATCH /repos/owner/repo/check-runs/77",
		"GET /repos/owner/repo/pulls/7",
		"GET /repos/owner/repo/pulls/7/reviews",
		"GET /repos/owner/repo/issues/7/comments",
		"GET /repos/owner/repo/pulls/7",
		"POST /repos/owner/repo/pulls/7/comments",
		"GET /repos/owner/repo/issues/7/comments",
		"POST /repos/owner/repo/issues/7/comments",
		"GET /repos/owner/repo/pulls/7",
		"POST /graphql",
		"POST /repos/owner/repo/pulls/7/reviews",
		"GET /repos/owner/repo/issues/7/comments",
		"PATCH /repos/owner/repo/issues/comments/2000",
		"PATCH /repos/owner/repo/check-runs/77",
	}
	assertRequestOrder(t, fixture.state.requestOrder, wantOrder)

	if fixture.state.lastSubmitReview == nil {
		t.Fatal("SubmitReview was not called")
	}
	// The verdict names the commit this run analyzed. Reloading the head and
	// comparing it closes most of the race, but a push can still land between
	// that check and this write, and a verdict pinned to the analyzed commit
	// cannot be read as judging the commit that replaced it.
	if fixture.state.lastSubmitReview["commit_id"] != testHeadSHA {
		t.Fatalf("commit_id = %v, want the analyzed commit %q",
			fixture.state.lastSubmitReview["commit_id"], testHeadSHA)
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "success" {
		t.Fatalf("conclusion = %v, want success", fixture.state.lastUpdateCheckRun["conclusion"])
	}
}

// Every write to the top level comment carries the state marker, because the
// marker is the only way the next run finds the comment. A body written
// without one makes that run open a second comment, and the write most likely
// to forget it is a failure notice, when the pull request is already in
// trouble.
func TestEveryTopLevelCommentWriteCarriesTheStateMarker(t *testing.T) {
	t.Run("an incomplete run then a completing run leave one comment", func(t *testing.T) {
		fixture := newServiceFixture(t, serviceFixtureOptions{
			model: &failThenSucceedModel{err: errors.New("provider refused the prompt")},
		})

		if err := fixture.run(context.Background(), fixture.job()); err != nil {
			t.Fatalf("first Run: %v", err)
		}
		pending, ok := marker.DecodeState(failureSummaryComment(t, fixture))
		if !ok || pending.Status != marker.StateReviewing || len(pending.Pending) != 1 {
			t.Fatalf("state after the unread chunk = %+v ok=%v, want a decodable reviewing marker", pending, ok)
		}

		if err := fixture.run(context.Background(), fixture.job()); err != nil {
			t.Fatalf("second Run: %v", err)
		}
		if len(fixture.state.issueComments) != 1 {
			t.Fatalf("issue comments = %d, want the failure comment reused, not a second one",
				len(fixture.state.issueComments))
		}
		body, ok := fixture.state.issueComments[0]["body"].(string)
		if !ok {
			t.Fatalf("summary comment body = %v, want a string", fixture.state.issueComments[0]["body"])
		}
		done, ok := marker.DecodeState(body)
		if !ok || done.Status != marker.StateDone {
			t.Fatalf("state after the success = %+v ok=%v, want a decodable done marker", done, ok)
		}
	})

	t.Run("a declined delta", func(t *testing.T) {
		fixture := newServiceFixture(t, serviceFixtureOptions{
			collector:       twoChunkCollector{},
			reviewMaxChunks: 1,
		})

		if err := fixture.run(context.Background(), fixture.job()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(fixture.state.issueComments) != 1 {
			t.Fatalf("issue comments = %d, want 1", len(fixture.state.issueComments))
		}
		body, ok := fixture.state.issueComments[0]["body"].(string)
		if !ok {
			t.Fatalf("summary comment body = %v, want a string", fixture.state.issueComments[0]["body"])
		}
		skipped, ok := marker.DecodeState(body)
		if !ok || skipped.Status != marker.StateSkipped {
			t.Fatalf("state = %+v ok=%v, want a decodable skipped marker", skipped, ok)
		}
	})
}

func TestServiceSkipsHeadWithExistingReviewMarker(t *testing.T) {
	head := domain.HeadSHA(testHeadSHA)
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reviewPages: [][]map[string]any{{
			{
				"id":        float64(11),
				"commit_id": string(head),
				"state":     "COMMENTED",
				"body":      marker.Review(head) + "\nExisting review.",
				"user":      map[string]any{"login": testBotLogin},
			},
		}},
	})

	err := fixture.run(context.Background(), fixture.job())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.reconciler.callCount != 0 {
		t.Fatalf("reconcile call count = %d, want 0", fixture.reconciler.callCount)
	}

	// The durable state is never read here. A head an existing review marker
	// already covers owes no delta, so the run pays for no issue comment read
	// on its way out.
	wantOrder := []string{
		"GET /repos/owner/repo/commits/a3c4f1cac7f595bc824704b9d2a1f1191630dc32/check-runs",
		"POST /repos/owner/repo/check-runs",
		"PATCH /repos/owner/repo/check-runs/77",
		"GET /repos/owner/repo/pulls/7",
		"GET /repos/owner/repo/pulls/7/reviews",
		"PATCH /repos/owner/repo/check-runs/77",
	}
	assertRequestOrder(t, fixture.state.requestOrder, wantOrder)
	if fixture.state.lastSubmitReview != nil {
		t.Fatal("SubmitReview was called for existing marker")
	}
}

// A head the durable state already records as reviewed, with no chunk pending,
// owes nothing. The run says so from the state alone rather than asking GitHub
// to compare a commit against itself.
func TestServiceReviewsNothingWhenTheStateAlreadyNamesTheHead(t *testing.T) {
	collector := &recordingDeltaCollector{}
	model := &sequenceModel{}
	fixture := newServiceFixture(t, serviceFixtureOptions{collector: collector, model: model})
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id": float64(1),
		"body": marker.EncodeState(marker.State{
			LastReviewed: domain.HeadSHA(testHeadSHA),
			RunID:        "delivery-0",
			Status:       marker.StateDone,
			Pending:      nil,
		}),
		"user": map[string]any{"login": testBotLogin},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(collector.bases()) != 0 {
		t.Fatalf("CollectRange calls = %v, want none: the state already names this head", collector.bases())
	}
	if model.callCount != 0 {
		t.Fatalf("model calls = %d, want none", model.callCount)
	}
	if fixture.reconciler.callCount != 0 {
		t.Fatalf("reconcile calls = %d, want none", fixture.reconciler.callCount)
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "success" {
		t.Fatalf("conclusion = %v, want success", fixture.state.lastUpdateCheckRun["conclusion"])
	}
}

func TestServiceIgnoresForeignReviewMarker(t *testing.T) {
	head := domain.HeadSHA(testHeadSHA)
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reviewPages: [][]map[string]any{{
			{
				"id":        float64(12),
				"commit_id": string(head),
				"state":     "COMMENTED",
				"body":      marker.Review(head) + "\nForeign review.",
				"user":      map[string]any{"login": "other-user"},
			},
		}},
	})

	err := fixture.run(context.Background(), fixture.job())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantOrder := []string{
		"GET /repos/owner/repo/commits/a3c4f1cac7f595bc824704b9d2a1f1191630dc32/check-runs",
		"POST /repos/owner/repo/check-runs",
		"PATCH /repos/owner/repo/check-runs/77",
		"GET /repos/owner/repo/pulls/7",
		"GET /repos/owner/repo/pulls/7/reviews",
		"GET /repos/owner/repo/issues/7/comments",
		"GET /repos/owner/repo/pulls/7",
		"POST /repos/owner/repo/pulls/7/comments",
		"GET /repos/owner/repo/issues/7/comments",
		"POST /repos/owner/repo/issues/7/comments",
		"GET /repos/owner/repo/pulls/7",
		"POST /graphql",
		"POST /repos/owner/repo/pulls/7/reviews",
		"GET /repos/owner/repo/issues/7/comments",
		"PATCH /repos/owner/repo/issues/comments/2000",
		"PATCH /repos/owner/repo/check-runs/77",
	}
	assertRequestOrder(t, fixture.state.requestOrder, wantOrder)
	if fixture.state.lastSubmitReview == nil {
		t.Fatal("SubmitReview was not called")
	}
}

// Reading a commit proves it was reviewed, not that it is still the head. A run
// whose head moved mid flight posts nothing more about that commit, submits no
// verdict, and never reads the threads it would have judged.
func TestServiceCancelsWhenHeadChangesBeforePublication(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		headAfterAnalysis: testStaleHeadSHA,
	})

	err := fixture.run(context.Background(), fixture.job())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.reconciler.callCount != 1 {
		t.Fatalf("reconcile call count = %d, want 1", fixture.reconciler.callCount)
	}

	// The head check that guards the first chunk's comments catches the move,
	// so the run ends there rather than posting to a commit nobody is reading.
	wantOrder := []string{
		"GET /repos/owner/repo/commits/a3c4f1cac7f595bc824704b9d2a1f1191630dc32/check-runs",
		"POST /repos/owner/repo/check-runs",
		"PATCH /repos/owner/repo/check-runs/77",
		"GET /repos/owner/repo/pulls/7",
		"GET /repos/owner/repo/pulls/7/reviews",
		"GET /repos/owner/repo/issues/7/comments",
		"GET /repos/owner/repo/pulls/7",
		"PATCH /repos/owner/repo/check-runs/77",
	}
	assertRequestOrder(t, fixture.state.requestOrder, wantOrder)
	if fixture.state.lastSubmitReview != nil {
		t.Fatal("SubmitReview was called after stale head")
	}
	if fixture.state.lastUpdateReview != nil {
		t.Fatal("a review was edited after the head moved, want no verdict at all")
	}
	// The head moved during the review, so the findings describe a commit
	// nobody is looking at any more and none of them post.
	if len(fixture.state.streamedComments) != 0 {
		t.Fatalf("streamed comments = %d, want none after the head moved",
			len(fixture.state.streamedComments))
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "cancelled" {
		t.Fatalf("conclusion = %v, want cancelled", fixture.state.lastUpdateCheckRun["conclusion"])
	}
}

func TestServiceFailsCheckWhenReviewPublicationFails(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		submitReviewStatus: http.StatusInternalServerError,
	})

	err := fixture.run(context.Background(), fixture.job())
	if err == nil {
		t.Fatal("Run: want error")
	}

	wantOrder := []string{
		"GET /repos/owner/repo/commits/a3c4f1cac7f595bc824704b9d2a1f1191630dc32/check-runs",
		"POST /repos/owner/repo/check-runs",
		"PATCH /repos/owner/repo/check-runs/77",
		"GET /repos/owner/repo/pulls/7",
		"GET /repos/owner/repo/pulls/7/reviews",
		"GET /repos/owner/repo/issues/7/comments",
		"GET /repos/owner/repo/pulls/7",
		"POST /repos/owner/repo/pulls/7/comments",
		"GET /repos/owner/repo/issues/7/comments",
		"POST /repos/owner/repo/issues/7/comments",
		"GET /repos/owner/repo/pulls/7",
		"POST /graphql",
		"POST /repos/owner/repo/pulls/7/reviews",
		"PATCH /repos/owner/repo/check-runs/77",
		"GET /repos/owner/repo/issues/7/comments",
		"PATCH /repos/owner/repo/issues/comments/2000",
	}
	assertRequestOrder(t, fixture.state.requestOrder, wantOrder)
	if fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf("conclusion = %v, want failure", fixture.state.lastUpdateCheckRun["conclusion"])
	}
}

func TestServiceCompletesCheckAfterReviewContextExpires(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: contextBlockingModel{},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	err := fixture.run(ctx, fixture.job())
	if err == nil {
		t.Fatal("Run: want timeout error")
	}
	if fixture.state.lastUpdateCheckRun["status"] != "completed" {
		t.Fatalf("status = %v, want completed", fixture.state.lastUpdateCheckRun["status"])
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf("conclusion = %v, want failure", fixture.state.lastUpdateCheckRun["conclusion"])
	}
}

func TestServiceCompletesCheckAfterModelPanic(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: panicModel{},
	})

	err := fixture.run(context.Background(), fixture.job())
	if err == nil {
		t.Fatal("Run: want panic error")
	}
	if fixture.state.lastUpdateCheckRun["status"] != "completed" {
		t.Fatalf("status = %v, want completed", fixture.state.lastUpdateCheckRun["status"])
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf("conclusion = %v, want failure", fixture.state.lastUpdateCheckRun["conclusion"])
	}
}

// The admission gate measures the delta before any model call and declines
// it outright when it is over budget. A declined review carries no verdict at
// all: no review submitted, none dismissed, only a notice saying why the
// review did not try.
//
// The check still must not pass. GitHub counts a required check concluded
// skipped as passing, so concluding that way would let an entirely unreviewed
// oversized delta merge with any earlier approval still standing.
func TestServiceDeclinesAnOverBudgetDeltaBeforeAnyModelCall(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:       twoChunkCollector{},
		reviewMaxChunks: 1,
	})

	err := fixture.run(context.Background(), fixture.job())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fixture.state.lastUpdateCheckRun["status"] != "completed" {
		t.Fatalf("status = %v, want completed", fixture.state.lastUpdateCheckRun["status"])
	}
	assertDeclinedCheckDoesNotPass(t, fixture)

	if len(fixture.state.issueComments) != 1 {
		t.Fatalf("issue comments = %d, want 1", len(fixture.state.issueComments))
	}
	body, ok := fixture.state.issueComments[0]["body"].(string)
	if !ok {
		t.Fatalf("summary comment body = %v, want a string", fixture.state.issueComments[0]["body"])
	}
	if !strings.Contains(body, "2 diff chunks") {
		t.Fatalf("summary comment body = %q, want the measured chunk count", body)
	}
	if !strings.Contains(body, "1 chunk review budget") {
		t.Fatalf("summary comment body = %q, want the configured budget", body)
	}

	if fixture.state.lastSubmitReview != nil {
		t.Fatalf("submitted review = %v, want none: a declined delta carries no verdict",
			fixture.state.lastSubmitReview)
	}
	if len(fixture.state.dismissals) != 0 {
		t.Fatalf("dismissals = %v, want none", fixture.state.dismissals)
	}
}

// passingCheckConclusions are the conclusions GitHub counts as satisfying a
// required check. A declined delta must reach none of them, or unreviewed code
// merges on the strength of having been refused a review.
var passingCheckConclusions = []string{"success", "skipped", "neutral"}

// assertDeclinedCheckDoesNotPass proves the admission gate leaves the merge gate
// shut, and says so as a class rather than pinning one string, so a later change
// to another passing conclusion fails here too.
func assertDeclinedCheckDoesNotPass(t *testing.T, fixture *serviceFixture) {
	t.Helper()
	conclusion, ok := fixture.state.lastUpdateCheckRun["conclusion"].(string)
	if !ok {
		t.Fatalf("conclusion = %v, want a string", fixture.state.lastUpdateCheckRun["conclusion"])
	}
	for _, passing := range passingCheckConclusions {
		if conclusion == passing {
			t.Fatalf("conclusion = %q, which GitHub counts as passing: an unreviewed delta could merge",
				conclusion)
		}
	}
	if conclusion != "action_required" {
		t.Fatalf("conclusion = %q, want action_required", conclusion)
	}
}

// The gate exists so an over budget delta costs nothing. Reconciliation makes
// its own model call and resolves threads, so running it before admission
// spent both on the exact delta the run was about to refuse, and mutated
// review state on the way.
func TestAnOverBudgetDeltaReconcilesNothingAndCallsNoModel(t *testing.T) {
	model := &sequenceModel{}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:       twoChunkCollector{},
		reviewMaxChunks: 1,
		model:           model,
		reconcileThreads: []githubapp.ReviewThread{{
			NodeID:   "open-thread",
			Resolved: false,
			RootComment: domain.ReviewComment{
				Author: testBotLogin,
				Body:   "an earlier finding the reconciler would look at",
				Path:   "main.go",
			},
		}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Thread resolution happens inside the reconciler, so a reconciler that was
	// never called resolved nothing.
	if fixture.reconciler.callCount != 0 {
		t.Fatalf("reconcile call count = %d, want 0 on a declined delta", fixture.reconciler.callCount)
	}
	if model.callCount != 0 {
		t.Fatalf("model call count = %d, want 0 on a declined delta", model.callCount)
	}
	assertDeclinedCheckDoesNotPass(t, fixture)
}

// budgetBypassCollector answers a compare the way GitHub would. Measured from
// the real baseline the range still carries the whole oversized delta; measured
// from the head that was declined it carries only the one commit pushed since.
//
// That difference is the bug this collector exists to catch. A skip that moved
// the baseline to the head it refused would be handed the small range on the
// next push, review it, approve, and let the oversized code merge unread.
type budgetBypassCollector struct {
	mu           sync.Mutex
	declinedHead domain.HeadSHA
	rangeBases   []domain.HeadSHA
}

func (collector *budgetBypassCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	base domain.HeadSHA,
) (diff.ReviewInput, error) {
	collector.mu.Lock()
	collector.rangeBases = append(collector.rangeBases, base)
	declined := collector.declinedHead
	collector.mu.Unlock()

	if base == declined {
		return paddedFiles(pullRequest, 1)
	}
	return paddedFiles(pullRequest, 2)
}

func (collector *budgetBypassCollector) bases() []domain.HeadSHA {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]domain.HeadSHA{}, collector.rangeBases...)
}

// A declined delta must not move the baseline to the head it refused.
//
// Advancing it there reopens the gate one push later by a different door than
// the conclusion does: the next run measures only the commits since a head
// nobody read, finds them small and clean, approves, and the oversized range
// merges with the marker claiming the whole range was reviewed. The check
// conclusion holds the gate only until that push.
//
// The spec says so and calls repeated skips the intended behaviour: an over
// budget delta stays over budget, and the way out is a person splitting the
// pull request, raising its budget, or asking for the review.
func TestADeclinedDeltaKeepsItsBaselineSoALaterPushCannotBypassTheBudget(t *testing.T) {
	const pushedHeadSHA = "d6f7a4fdfaf828ef157a37e2f5d4f4424963af65"

	collector := &budgetBypassCollector{declinedHead: domain.HeadSHA(testHeadSHA)}
	model := &sequenceModel{}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:       collector,
		reviewMaxChunks: 1,
		model:           model,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	assertDeclinedCheckDoesNotPass(t, fixture)
	declined := decodedSummaryState(t, fixture)
	if declined.LastReviewed != "" {
		t.Fatalf("last reviewed after the skip = %q, want it left where it was: nothing was read",
			declined.LastReviewed)
	}
	if declined.Status != marker.StateSkipped {
		t.Fatalf("status = %q, want %q", declined.Status, marker.StateSkipped)
	}

	// One small commit lands on top of the delta that was refused.
	fixture.state.mu.Lock()
	fixture.state.headSHA = pushedHeadSHA
	fixture.state.mu.Unlock()

	pushed := fixture.job()
	pushed.DeliveryID = "delivery-2"
	pushed.Head = domain.HeadSHA(pushedHeadSHA)
	if err := fixture.run(context.Background(), pushed); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	bases := collector.bases()
	if len(bases) != 2 {
		t.Fatalf("CollectRange calls = %v, want one per run", bases)
	}
	if bases[1] != "" {
		t.Fatalf("second compare base = %q, want the baseline unmoved: measuring from the declined head "+
			"hides the oversized range and merges it unreviewed", bases[1])
	}
	assertDeclinedCheckDoesNotPass(t, fixture)
	if model.callCount != 0 {
		t.Fatalf("model call count = %d, want 0: the oversized range is still over budget", model.callCount)
	}
	if fixture.state.lastSubmitReview != nil {
		t.Fatalf("submitted review = %v, want none: nothing was reviewed", fixture.state.lastSubmitReview)
	}
	if after := decodedSummaryState(t, fixture); after.LastReviewed != "" {
		t.Fatalf("last reviewed after the push = %q, want it still unmoved", after.LastReviewed)
	}
}

// A redelivery of a declined head must be declined again.
//
// This door needs no push at all. A baseline advanced to the head that was
// refused makes the next run find the state already naming this head with
// nothing pending, which returns "already reviewed" and concludes the check
// successfully, so a plain webhook retry opens the gate over code nobody read.
func TestARedeliveryOfADeclinedHeadIsDeclinedAgain(t *testing.T) {
	model := &sequenceModel{}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:       twoChunkCollector{},
		reviewMaxChunks: 1,
		model:           model,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	assertDeclinedCheckDoesNotPass(t, fixture)

	// The same delivery arrives again, which is what GitHub does on a retry.
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("redelivered Run: %v", err)
	}

	assertDeclinedCheckDoesNotPass(t, fixture)
	if title := fmt.Sprint(checkOutput(t, fixture)["title"]); title == checkTitleAlreadyReviewedText {
		t.Fatalf("check title = %q, want the skip: nothing was ever reviewed on this head", title)
	}
	if model.callCount != 0 {
		t.Fatalf("model call count = %d, want 0: the delta is still over budget", model.callCount)
	}
	if fixture.reconciler.callCount != 0 {
		t.Fatalf("reconcile call count = %d, want 0 on a redelivered decline", fixture.reconciler.callCount)
	}
	if state := decodedSummaryState(t, fixture); state.Status != marker.StateSkipped {
		t.Fatalf("status = %q, want %q", state.Status, marker.StateSkipped)
	}
}

// checkTitleAlreadyReviewedText is the title the service completes a check with
// when it finds nothing owed. A declined delta must never reach it.
const checkTitleAlreadyReviewedText = "Already reviewed"

// A declined delta must keep the work an earlier run already recorded. It read
// no chunk, so it has nothing to add and no business taking any away: erasing
// the finished chunk list would send the next run that is allowed to review
// over work this pull request already paid for.
func TestADeclinedDeltaKeepsTheWorkAnEarlierRunRecorded(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:       twoChunkCollector{},
		reviewMaxChunks: 1,
		model:           &sequenceModel{},
	})
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id": float64(1),
		"body": "## Review\n\nan earlier run\n\n" + marker.EncodeState(marker.State{
			LastReviewed: domain.HeadSHA(testStaleHeadSHA),
			RunID:        "delivery-0",
			Status:       marker.StateReviewing,
			Pending:      []string{"aaaaaaaaaaaa"},
			Completed:    []string{"bbbbbbbbbbbb", "cccccccccccc"},
		}),
		"user": map[string]any{"login": testBotLogin},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertDeclinedCheckDoesNotPass(t, fixture)
	state := decodedSummaryState(t, fixture)
	if state.LastReviewed != domain.HeadSHA(testStaleHeadSHA) {
		t.Fatalf("last reviewed = %q, want the earlier run's commit %q", state.LastReviewed, testStaleHeadSHA)
	}
	if len(state.Completed) != 2 {
		t.Fatalf("completed = %v, want the two chunks an earlier run finished", state.Completed)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending = %v, want the chunk an earlier run still owed", state.Pending)
	}
	if state.Status != marker.StateSkipped {
		t.Fatalf("status = %q, want %q", state.Status, marker.StateSkipped)
	}
	if state.RunID != "delivery-1" {
		t.Fatalf("run id = %q, want the run that declined the delta", state.RunID)
	}
}

// A skip that cannot be written is still a run that has to end. Before this
// the error travelled back to the dispatcher, which only logs, so a GitHub
// outage during a skip left the check in progress forever with nothing on the
// pull request saying why.
func TestAFailedSkipWriteStillCompletesTheCheck(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:          twoChunkCollector{},
		reviewMaxChunks:    1,
		issueCommentStatus: http.StatusInternalServerError,
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want the failed skip write surfaced")
	}

	if fixture.state.lastUpdateCheckRun["status"] != "completed" {
		t.Fatalf("status = %v, want completed", fixture.state.lastUpdateCheckRun["status"])
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf("conclusion = %v, want failure", fixture.state.lastUpdateCheckRun["conclusion"])
	}
	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	if output["title"] != "Review failed while recording the skipped review." {
		t.Fatalf("title = %v, want the skip write stage", output["title"])
	}
}

// A zero budget refuses every real delta while admitting an empty one, which
// is the opposite of what a budget is for. It means the caller left the value
// unset, so the configured default applies.
func TestUnsetReviewBudgetsAdmitANormalDelta(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		unsetReviewBudgets: true,
		model: &sequenceModel{
			results: []domain.ReviewResult{{CoverageComplete: true, Findings: nil}},
		},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "success" {
		t.Fatalf("conclusion = %v, want success: a one file delta is inside the default budget",
			fixture.state.lastUpdateCheckRun["conclusion"])
	}
	if fixture.state.lastSubmitReview == nil {
		t.Fatal("no review was submitted, want the delta reviewed rather than declined")
	}
}

func TestServiceCreatesAndCompletesCheckBeforePullRequestLoadFails(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		pullRequestStatus: http.StatusInternalServerError,
	})

	err := fixture.run(context.Background(), fixture.job())
	if err == nil {
		t.Fatal("Run: want pull request error")
	}
	if len(fixture.state.checkRuns) != 1 {
		t.Fatalf("check runs = %d, want 1", len(fixture.state.checkRuns))
	}
	if fixture.state.lastUpdateCheckRun["status"] != "completed" {
		t.Fatalf("status = %v, want completed", fixture.state.lastUpdateCheckRun["status"])
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf("conclusion = %v, want failure", fixture.state.lastUpdateCheckRun["conclusion"])
	}
}

func TestServiceFailsBeforePublicationWhenReconciliationFails(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reconcileErr: errors.New("reconcile failed"),
	})

	err := fixture.run(context.Background(), fixture.job())
	if err == nil {
		t.Fatal("Run: want error")
	}
	if fixture.reconciler.callCount != 1 {
		t.Fatalf("reconcile call count = %d, want 1", fixture.reconciler.callCount)
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf("conclusion = %v, want failure", fixture.state.lastUpdateCheckRun["conclusion"])
	}
	if fixture.state.lastSubmitReview != nil {
		t.Fatalf("submitted review = %v, want none on a failure", fixture.state.lastSubmitReview)
	}
	assertSanitizedFailureComment(
		t,
		fixture,
		"Review failed while reconciling existing findings.",
		"reconcile failed",
	)
}

// failureSummaryComment returns the body of the one top level comment a failed
// run leaves behind, which is where the failure is now reported.
func failureSummaryComment(t *testing.T, fixture *serviceFixture) string {
	t.Helper()
	if len(fixture.state.issueComments) != 1 {
		t.Fatalf("issue comments = %d, want the one summary comment", len(fixture.state.issueComments))
	}
	body, ok := fixture.state.issueComments[0]["body"].(string)
	if !ok {
		t.Fatalf("summary comment body = %v, want a string", fixture.state.issueComments[0]["body"])
	}
	return body
}

// assertSanitizedFailureComment proves the comment says the review stopped and
// where the cause is, and that the provider's own words are not in it.
//
// A model provider error can carry the request it failed on, an internal
// endpoint, or a credential. A pull request comment is public and permanent,
// so the cause goes to the service log and the comment carries the run
// identifier that finds it.
func assertSanitizedFailureComment(t *testing.T, fixture *serviceFixture, statedFailure string, rawCause string) {
	t.Helper()
	body := failureSummaryComment(t, fixture)
	if !strings.Contains(body, statedFailure) {
		t.Fatalf("summary comment = %q, want the stated failure %q", body, statedFailure)
	}
	if !strings.Contains(body, "under run identifier `delivery-1`") {
		t.Fatalf("summary comment = %q, want the run identifier that finds the cause", body)
	}
	if strings.Contains(body, rawCause) {
		t.Fatalf("summary comment published the raw provider cause %q:\n%s", rawCause, body)
	}
}

// checkOutput returns the output object of the last check run update.
func checkOutput(t *testing.T, fixture *serviceFixture) map[string]any {
	t.Helper()
	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("check output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	return output
}

// A check run is as public and as permanent as a pull request comment, so it
// names which chunks went unread and where the cause is, and never quotes the
// provider. The cause itself stays in the service log the run identifier finds.
func TestTheNeutralCheckNamesTheUnreadChunksWithoutQuotingTheProvider(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector: twoChunkCollector{},
		model:     &sequenceModel{err: errors.New("provider rejected response schema")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	output := checkOutput(t, fixture)
	if !strings.Contains(fmt.Sprint(output["title"]), "could not be reviewed") {
		t.Fatalf("title = %v, want the count of unread chunks", output["title"])
	}
	summary, ok := output["summary"].(string)
	if !ok {
		t.Fatalf("summary = %v, want string", output["summary"])
	}
	if strings.Contains(summary, "provider rejected response schema") {
		t.Fatalf("check summary published the raw provider cause:\n%s", summary)
	}
	for _, want := range []string{
		"Chunks left unread: 1, 2.",
		"under run identifier `delivery-1`",
		"<summary>Review details</summary>",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("check summary missing %q:\n%s", want, summary)
		}
	}
}

// Exhausted usage is the largest single failure cause in production, so both
// public surfaces say so in this service's own words. The provider's message
// behind it reaches neither of them.
func TestServiceNamesUsageExhaustionInCheckAndNotice(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &sequenceModel{err: quotaExhaustedError{}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantReason := "Review stopped: the model provider reported no remaining usage."
	checkSummary, ok := checkOutput(t, fixture)["summary"].(string)
	if !ok || !strings.Contains(checkSummary, wantReason) {
		t.Fatalf("check summary = %v, want the classified reason", checkOutput(t, fixture)["summary"])
	}
	if strings.Contains(checkSummary, "usage credits are exhausted") {
		t.Fatalf("check summary published the raw provider cause:\n%s", checkSummary)
	}
	assertSanitizedFailureComment(t, fixture, wantReason, "usage credits are exhausted")
}

// A run that could not read every chunk leaves no review marker, so the same
// head is reviewed again rather than being suppressed as already done.
func TestAnIncompleteRunOmitsTheReviewMarkerSoTheHeadIsReviewedAgain(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &sequenceModel{err: errors.New("provider rejected response schema")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	body := failureSummaryComment(t, fixture)
	if _, found := marker.FindReview(body); found {
		t.Fatalf("summary comment = %q, want no review marker", body)
	}
}

// The failure notice edits the one comment the service already owns rather
// than leaving a second one behind, so a pull request that fails repeatedly
// still shows one summary.
func TestServiceFailureNoticeEditsTheExistingSummaryComment(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reconcileErr: errors.New("reconcile failed"),
	})
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id": float64(1),
		"body": "## Review\n\nNo severe findings.\n\n" + marker.EncodeState(marker.State{
			LastReviewed: domain.HeadSHA(testStaleHeadSHA),
			RunID:        "delivery-0",
			Status:       marker.StateDone,
			Pending:      nil,
		}),
		"user": map[string]any{"login": testBotLogin},
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want error")
	}
	if fixture.state.issueCommentUpdates != 1 {
		t.Fatalf("issue comment updates = %d, want the existing comment edited once", fixture.state.issueCommentUpdates)
	}
	body := failureSummaryComment(t, fixture)
	if !strings.Contains(body, "Review failed while reconciling existing findings.") {
		t.Fatalf("summary comment = %q, want the failure wording", body)
	}
}

// A run that stops early must report the same detail table a finished one does,
// so a reader can tell which model answered and how far it got.
func TestAnIncompleteRunReportsProgressInTheCommentAndTheCheck(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &sequenceModel{err: errors.New("provider refused the prompt")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body := failureSummaryComment(t, fixture)
	for _, want := range []string{
		"<summary>Review details</summary>",
		// No model answered, because the only chunk failed. The table says so
		// rather than naming the configured model that never replied.
		"| Model | unknown |",
		"| Head | `a3c4f1c` |",
		"| Files reviewed | `1` |",
		"| Diff chunks | `1` |",
		// A chunk nobody read is a slice of the head nobody covered, so the
		// table says so rather than claiming coverage it never established.
		"| Coverage complete | no |",
		"1 chunk could not be reviewed",
		// The block names what is holding it, the unread chunk included.
		"This head was not fully reviewed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("incomplete notice missing %q:\n%s", want, body)
		}
	}
	// The progress table is this service's own measurements, so the comment
	// carries all of it. The provider's message is not part of it.
	if strings.Contains(body, "provider refused the prompt") {
		t.Fatalf("summary comment published the raw provider cause:\n%s", body)
	}
	if !strings.Contains(body, "under run identifier `delivery-1`") {
		t.Fatalf("summary comment = %q, want the run identifier that finds the cause", body)
	}

	checkSummary, ok := checkOutput(t, fixture)["summary"].(string)
	if !ok {
		t.Fatalf("check summary = %v, want string", checkOutput(t, fixture)["summary"])
	}
	if strings.Contains(checkSummary, "provider refused the prompt") {
		t.Fatalf("check summary published the raw provider cause:\n%s", checkSummary)
	}
	if !strings.Contains(checkSummary, "under run identifier `delivery-1`") {
		t.Fatalf("check summary lost the pointer to the cause:\n%s", checkSummary)
	}
	for _, want := range []string{"Chunks left unread: 1.", "| Coverage complete | no |"} {
		if !strings.Contains(checkSummary, want) {
			t.Fatalf("check summary missing %q:\n%s", want, checkSummary)
		}
	}
}

// A review that fails before it reads anything still says so, rather than
// reporting an empty table the reader cannot interpret.
func TestServiceFailureBeforeAnyProgressReportsNothingReached(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		pullRequestStatus: http.StatusInternalServerError,
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want error")
	}
	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("check output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	checkSummary, ok := output["summary"].(string)
	if !ok || !strings.Contains(checkSummary, "| Reached | nothing |") {
		t.Fatalf("check summary = %v, want a nothing reached row", output["summary"])
	}
}

func TestServicePublishesAFailureNoticeForEveryReachableStage(t *testing.T) {
	stages := []struct {
		name    string
		title   string
		options serviceFixtureOptions
	}{
		{
			name:    "pull request load",
			title:   "Review failed while loading the pull request.",
			options: serviceFixtureOptions{pullRequestStatus: http.StatusInternalServerError},
		},
		{
			name:    "reconciliation",
			title:   "Review failed while reconciling existing findings.",
			options: serviceFixtureOptions{reconcileErr: errors.New("reconcile failed")},
		},
		{
			name:    "diff collection",
			title:   "Review failed while collecting the pull request diff.",
			options: serviceFixtureOptions{collector: failingCollector{}},
		},
		{
			name:    "head refresh",
			title:   "Review failed while refreshing the pull request head.",
			options: serviceFixtureOptions{pullRequestStatusAfterFirstRead: http.StatusInternalServerError},
		},
		{
			name:    "panic",
			title:   "Review failed after an internal panic.",
			options: serviceFixtureOptions{model: panicModel{}},
		},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			fixture := newServiceFixture(t, stage.options)

			if err := fixture.run(context.Background(), fixture.job()); err == nil {
				t.Fatal("Run: want error")
			}

			output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
			if !ok {
				t.Fatalf("output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
			}
			if output["title"] != stage.title {
				t.Fatalf("title = %v, want %q", output["title"], stage.title)
			}
			if fixture.state.lastSubmitReview != nil {
				t.Fatalf("submitted review = %v, want none on a failure", fixture.state.lastSubmitReview)
			}
			if body := failureSummaryComment(t, fixture); !strings.Contains(body, stage.title) {
				t.Fatalf("summary comment = %q, want %q", body, stage.title)
			}
		})
	}
}

func TestServiceRejectReportsTheFailureInTheSummaryComment(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})

	admitted, err := fixture.service.Admit(context.Background(), fixture.job())
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := fixture.service.Reject(
		context.Background(),
		admitted,
		errors.New("review queue is full"),
	); err == nil {
		t.Fatal("Reject: want the rejection cause")
	}

	assertSanitizedFailureComment(t, fixture, "Review failed.", "review queue is full")
	if fixture.state.lastSubmitReview != nil {
		t.Fatalf("submitted review = %v, want none on a rejection", fixture.state.lastSubmitReview)
	}
}

// A failure inside publication is reported by the check run and the comment,
// and still changes no review object: the review that failed to submit or
// update stays exactly as the reader last saw it.
func TestServiceReportsAPublicationFailureWithoutChangingAnyReview(t *testing.T) {
	summaryReview := map[string]any{
		"id":        float64(42),
		"commit_id": testStaleHeadSHA,
		"body":      "## Review\n\nNo severe findings.\n\n" + marker.Summary(),
		"state":     "COMMENTED",
		"user":      map[string]any{"login": testBotLogin},
	}
	cases := []struct {
		name    string
		title   string
		options serviceFixtureOptions
	}{
		{
			name:    "review reads fail",
			title:   "Review failed while reading existing reviews.",
			options: serviceFixtureOptions{reviewListStatus: http.StatusInternalServerError},
		},
		{
			name:  "summary update fails",
			title: "Review failed while updating the visible summary.",
			options: serviceFixtureOptions{
				updateReviewStatus: http.StatusInternalServerError,
				reviewPages:        [][]map[string]any{{summaryReview}},
			},
		},
		{
			name:    "review submission fails",
			title:   "Review failed while publishing the final decision.",
			options: serviceFixtureOptions{submitReviewStatus: http.StatusInternalServerError},
		},
		{
			name:    "thread read fails",
			title:   "Review failed while reading the open review threads.",
			options: serviceFixtureOptions{listThreadsStatus: http.StatusInternalServerError},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newServiceFixture(t, testCase.options)

			if err := fixture.run(context.Background(), fixture.job()); err == nil {
				t.Fatal("Run: want error")
			}
			output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
			if !ok {
				t.Fatalf("output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
			}
			if output["title"] != testCase.title {
				t.Fatalf("title = %v, want %q", output["title"], testCase.title)
			}
			if len(fixture.state.dismissals) != 0 {
				t.Fatalf("dismissals = %v, want none", fixture.state.dismissals)
			}
			if body := failureSummaryComment(t, fixture); !strings.Contains(body, testCase.title) {
				t.Fatalf("summary comment = %q, want %q", body, testCase.title)
			}
		})
	}
}

func TestServiceReturnsTheReviewCauseWhenTheNoticeCannotBeWritten(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reconcileErr:       errors.New("reconcile failed"),
		issueCommentStatus: http.StatusInternalServerError,
	})

	err := fixture.run(context.Background(), fixture.job())
	if err == nil {
		t.Fatal("Run: want error")
	}
	if !strings.Contains(err.Error(), "reconcile failed") {
		t.Fatalf("err = %q, want the review cause", err)
	}
	if strings.Contains(err.Error(), "issue comment") {
		t.Fatalf("err = %q, want the notice failure kept out of the returned cause", err)
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf("conclusion = %v, want failure even when the notice cannot be written",
			fixture.state.lastUpdateCheckRun["conclusion"])
	}
}

// A run that leaves a chunk pending leaves no review marker for its head, so
// the same head is reviewed again on the next attempt and the one comment is
// rewritten in place.
func TestServiceReviewsTheSameHeadAgainAfterAnIncompleteRun(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &failThenSucceedModel{err: errors.New("provider refused the prompt")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if !strings.Contains(failureSummaryComment(t, fixture), "could not be reviewed") {
		t.Fatalf("summary comment = %q, want the unread chunk wording", failureSummaryComment(t, fixture))
	}

	// The retry runs against the neutral check the first attempt left behind,
	// exactly as a redelivery does. Only a completed successful check
	// suppresses a rerun, so this one does not.
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "success" {
		t.Fatalf("conclusion = %v, want success on the retry", fixture.state.lastUpdateCheckRun["conclusion"])
	}
	if len(fixture.state.issueComments) != 1 {
		t.Fatalf("issue comments = %d, want the one comment rewritten in place", len(fixture.state.issueComments))
	}
	rewritten, ok := fixture.state.issueComments[0]["body"].(string)
	if !ok || strings.Contains(rewritten, "could not be reviewed") {
		t.Fatalf("rewritten comment = %v, want the normal review summary", fixture.state.issueComments[0]["body"])
	}
	if fixture.state.lastSubmitReview == nil {
		t.Fatal("the retry did not publish its review")
	}
}

// A normal review run leaves exactly one summary comment behind, carrying the
// durable state marker a later invocation resumes from. The create-then-
// update-in-place contract of upsertSummaryComment itself is proven directly
// in TestUpsertSummaryCommentCreatesOnceThenUpdatesInPlace, because a second
// call to fixture.run on the same head here hits the pre-existing completed
// check dedup path (checkAlreadySucceeded) rather than a repeat publish.
func TestServiceCreatesTheSummaryCommentCarryingTheStateMarker(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fixture.state.issueComments) != 1 {
		t.Fatalf("issue comments = %d, want 1", len(fixture.state.issueComments))
	}
	body, ok := fixture.state.issueComments[0]["body"].(string)
	if !ok {
		t.Fatalf("summary comment body = %v, want a string", fixture.state.issueComments[0]["body"])
	}
	state, ok := marker.DecodeState(body)
	if !ok {
		t.Fatalf("summary comment body = %q, want a decodable state marker", body)
	}
	if state.LastReviewed != domain.HeadSHA(testHeadSHA) {
		t.Fatalf("last reviewed = %q, want %q", state.LastReviewed, testHeadSHA)
	}
	if state.Status != marker.StateDone {
		t.Fatalf("status = %q, want %q", state.Status, marker.StateDone)
	}
}

// A pull request whose state marker names an earlier commit must cause the
// run to request the compare range from that commit to the head, not the
// full changed file list, so a run never reviews the same commit range
// twice.
func TestServiceRequestsTheDeltaSinceTheLastReviewedCommitWhenAMarkerExists(t *testing.T) {
	collector := &recordingDeltaCollector{}
	fixture := newServiceFixture(t, serviceFixtureOptions{collector: collector})
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id": float64(1),
		"body": marker.EncodeState(marker.State{
			LastReviewed: domain.HeadSHA(testStaleHeadSHA),
			RunID:        "r1",
			Status:       marker.StateDone,
			Pending:      nil,
		}),
		"user": map[string]any{"login": testBotLogin},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	bases := collector.bases()
	if len(bases) != 1 {
		t.Fatalf("CollectRange calls = %d, want 1", len(bases))
	}
	if bases[0] != domain.HeadSHA(testStaleHeadSHA) {
		t.Fatalf("compare base = %q, want the last reviewed commit %q", bases[0], testStaleHeadSHA)
	}
}

// Anyone who can comment on a pull request can write a state marker, so the
// marker alone is not a credential. A forged one naming the head would make
// the run compare the head against itself and review nothing, which is a
// review bypass. Authorship is the gate; the marker only locates the comment
// behind it.
func TestServiceIgnoresAStateMarkerFromAnotherAuthor(t *testing.T) {
	collector := &recordingDeltaCollector{}
	fixture := newServiceFixture(t, serviceFixtureOptions{collector: collector})
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id": float64(1),
		"body": marker.EncodeState(marker.State{
			LastReviewed: domain.HeadSHA(testHeadSHA),
			RunID:        "forged",
			Status:       marker.StateDone,
			Pending:      nil,
		}),
		"user": map[string]any{"login": "someone-else"},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	bases := collector.bases()
	if len(bases) != 1 || bases[0] != domain.HeadSHA("") {
		t.Fatalf("compare bases = %v, want one full collection: a foreign marker is not this service's state", bases)
	}
	if len(fixture.state.issueComments) != 2 {
		t.Fatalf("issue comments = %d, want the foreign one plus the service's own", len(fixture.state.issueComments))
	}
	foreign, ok := fixture.state.issueComments[0]["body"].(string)
	if !ok || !strings.Contains(foreign, "run=forged") {
		t.Fatalf("foreign comment = %v, want it left exactly as it was",
			fixture.state.issueComments[0]["body"])
	}
}

// No state marker means this head has never been reviewed, so the run must
// request the full file list rather than comparing against nothing.
func TestServiceRequestsTheFullFileListWhenNoStateMarkerExists(t *testing.T) {
	collector := &recordingDeltaCollector{}
	fixture := newServiceFixture(t, serviceFixtureOptions{collector: collector})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	bases := collector.bases()
	if len(bases) != 1 {
		t.Fatalf("CollectRange calls = %d, want 1", len(bases))
	}
	if bases[0] != domain.HeadSHA("") {
		t.Fatalf("compare base = %q, want empty on first contact", bases[0])
	}
}

type failingCollector struct{}

func (failingCollector) CollectRange(
	context.Context,
	domain.PullRequestRef,
	githubapp.PullRequest,
	domain.HeadSHA,
) (diff.ReviewInput, error) {
	return diff.ReviewInput{}, errors.New("collect diff failed")
}

// failThenSucceedModel fails the first review pass and succeeds afterwards.
type failThenSucceedModel struct {
	err   error
	calls int
}

func (model *failThenSucceedModel) Review(context.Context, string) (review.Completion, error) {
	model.calls++
	if model.calls == 1 {
		return review.Completion{}, model.err
	}
	return review.Completion{
		Result: domain.ReviewResult{CoverageComplete: true, Findings: nil},
		Model:  testReviewModel,
	}, nil
}

// quotaExhaustedError mimics the provider error shape for exhausted usage.
type quotaExhaustedError struct{}

func (quotaExhaustedError) Error() string {
	return "model provider returned HTTP 400 Bad Request: invalid_request_error: " +
		"upstream_failed: upstream call failed: usage credits are exhausted"
}

func (quotaExhaustedError) UsageExceeded() bool {
	return true
}

// A finding an earlier review already carried stays suppressed even when this
// run rewords it, because its thread is already on the page. Every other
// finding is published: nothing else withholds one.
func TestServiceSuppressesAHistoricalFindingAndPublishesTheRest(t *testing.T) {
	historical := domain.Finding{
		Path:       "main.go",
		StartLine:  4,
		EndLine:    4,
		Title:      "Historical defect",
		Body:       "Original wording.",
		Importance: 9,
	}
	historicalBody, err := marker.EncodeFindingBody(domain.HeadSHA(testStaleHeadSHA), historical)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}

	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		reconcileThreads: []githubapp.ReviewThread{{
			NodeID:   "historical-thread",
			Resolved: true,
			RootComment: domain.ReviewComment{
				Author:    testBotLogin,
				Body:      historicalBody,
				Path:      historical.Path,
				StartLine: historical.StartLine,
				EndLine:   historical.EndLine,
			},
		}},
		model: &sequenceModel{results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings: []domain.Finding{
				{
					Path:       "main.go",
					StartLine:  2,
					EndLine:    4,
					Title:      "Reworded historical defect",
					Body:       "New wording must not republish this finding.",
					Importance: 10,
				},
				{
					Path:       "main.go",
					StartLine:  2,
					EndLine:    2,
					Title:      "Second defect",
					Body:       "A different defect on a different line.",
					Importance: 9,
				},
				{
					Path:       "main.go",
					StartLine:  3,
					EndLine:    3,
					Title:      "Third defect",
					Body:       "A third defect on a third line.",
					Importance: 10,
				},
			},
		}}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v, want REQUEST_CHANGES", fixture.state.lastSubmitReview["event"])
	}
	if len(fixture.state.streamedComments) != 2 {
		t.Fatalf("streamed comments = %d, want both new defects and not the reworded historical one",
			len(fixture.state.streamedComments))
	}
	for _, body := range bodiesOf(fixture.state.streamedComments) {
		if strings.Contains(body, "Reworded historical defect") {
			t.Fatalf("the historical finding was republished:\n%s", body)
		}
	}
}

// An unresolved thread of the service's own keeps the verdict blocking, and it
// no longer stops this run publishing what it just found: nothing rations
// comments against a thread count any more.
func TestServiceKeepsPublishingWhileAnEarlierThreadIsOpen(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings: []domain.Finding{{
				Path:       "main.go",
				StartLine:  2,
				EndLine:    2,
				Title:      "Current severe defect",
				Body:       "The defect still requires a blocking decision.",
				Importance: 9,
			}},
		}}},
		reconcileThreads: []githubapp.ReviewThread{{
			NodeID:   "existing-thread",
			Resolved: false,
			RootComment: domain.ReviewComment{
				Author: testBotLogin,
				Body:   "existing bot thread",
			},
		}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v, want REQUEST_CHANGES", fixture.state.lastSubmitReview["event"])
	}
	if len(fixture.state.streamedComments) != 1 {
		t.Fatalf("streamed comments = %d, want the new defect published alongside the open thread",
			len(fixture.state.streamedComments))
	}
	comments, ok := fixture.state.lastSubmitReview["comments"].([]any)
	if !ok || len(comments) != 0 {
		t.Fatalf("comments = %v, want none: they posted as their chunk answered",
			fixture.state.lastSubmitReview["comments"])
	}
}

// A finding published once stays suppressed after its thread is resolved, so a
// later run publishes nothing while the model still reports the same defect.
// Requesting changes there leaves a blocking review with no open thread and
// nothing to fix, and only a human dismissal clears it.
func TestServiceApprovesWhenEveryFindingIsAlreadyResolved(t *testing.T) {
	resolvedFinding := domain.Finding{
		Path:       "main.go",
		StartLine:  2,
		EndLine:    2,
		Title:      "Severe defect",
		Body:       "The changed line breaks core behavior.",
		Importance: 9,
	}
	findingMarker, err := marker.Finding(domain.HeadSHA(testHeadSHA), resolvedFinding)
	if err != nil {
		t.Fatalf("Finding marker: %v", err)
	}

	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings:         []domain.Finding{resolvedFinding},
		}}},
		reconcileThreads: []githubapp.ReviewThread{{
			NodeID:   "resolved-thread",
			Resolved: true,
			RootComment: domain.ReviewComment{
				Author:    testBotLogin,
				Body:      "The changed line breaks core behavior.\n\n" + findingMarker,
				Path:      resolvedFinding.Path,
				StartLine: resolvedFinding.StartLine,
				EndLine:   resolvedFinding.EndLine,
			},
		}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("event = %v, want APPROVE because nothing was published and no thread is open",
			fixture.state.lastSubmitReview["event"])
	}
	comments, ok := fixture.state.lastSubmitReview["comments"].([]any)
	if !ok || len(comments) != 0 {
		t.Fatalf("comments = %v, want none", fixture.state.lastSubmitReview["comments"])
	}
}

// A run that posts a new finding must never approve over it.
//
// The verdict reads the service's own threads after publication, so the thread
// this run just opened is one of its inputs. Reading them before analysis
// would show none of this run's findings and approve the very defects it had
// raised minutes earlier.
func TestARunThatPostsANewFindingDoesNotApprove(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings: []domain.Finding{{
				Path:       "main.go",
				StartLine:  2,
				EndLine:    2,
				Title:      "Severe defect",
				Body:       "The changed line breaks core behavior.",
				Importance: 9,
			}},
		}}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fixture.state.streamedComments) != 1 {
		t.Fatalf("streamed comments = %d, want the finding posted", len(fixture.state.streamedComments))
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v, want REQUEST_CHANGES over the thread this run opened",
			fixture.state.lastSubmitReview["event"])
	}
}

// A blocking verdict must say what is holding it.
//
// A run that finds nothing new still blocks while an earlier thread is open,
// and with nothing said about it the review reads as a silent repeat. One live
// pull request carried three blocking reviews, two of them empty, and no
// reader could tell that one unresolved thread was the whole cause.
func TestABlockingVerdictNamesTheOpenThreadsHoldingIt(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model: &sequenceModel{
			results: []domain.ReviewResult{{CoverageComplete: true, Findings: nil}},
		},
		reconcileThreads: []githubapp.ReviewThread{{
			NodeID:   "open-thread",
			Resolved: false,
			RootComment: domain.ReviewComment{
				DatabaseID: 4242,
				Author:     testBotLogin,
				Body:       "The changed line breaks core behavior.",
				Path:       "main.go",
				StartLine:  2,
				EndLine:    2,
			},
		}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v, want REQUEST_CHANGES", fixture.state.lastSubmitReview["event"])
	}

	body, ok := fixture.state.issueComments[0]["body"].(string)
	if !ok {
		t.Fatalf("summary comment body = %v, want a string", fixture.state.issueComments[0]["body"])
	}
	for _, want := range []string{
		"Waiting on:",
		"main.go:2",
		"https://github.com/owner/repo/pull/7#discussion_r4242",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("summary comment missing %q:\n%s", want, body)
		}
	}
}

// One open bot thread is a live objection, so the review keeps blocking even
// when this run publishes no new comment.
func TestServiceKeepsRequestingChangesWhileABotThreadStaysOpen(t *testing.T) {
	openFinding := domain.Finding{
		Path:       "main.go",
		StartLine:  2,
		EndLine:    2,
		Title:      "Severe defect",
		Body:       "The changed line breaks core behavior.",
		Importance: 9,
	}
	findingMarker, err := marker.Finding(domain.HeadSHA(testHeadSHA), openFinding)
	if err != nil {
		t.Fatalf("Finding marker: %v", err)
	}

	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings:         []domain.Finding{openFinding},
		}}},
		reconcileThreads: []githubapp.ReviewThread{{
			NodeID:   "open-thread",
			Resolved: false,
			RootComment: domain.ReviewComment{
				Author:    testBotLogin,
				Body:      "The changed line breaks core behavior.\n\n" + findingMarker,
				Path:      openFinding.Path,
				StartLine: openFinding.StartLine,
				EndLine:   openFinding.EndLine,
			},
		}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionRequestChanges) {
		t.Fatalf("event = %v, want REQUEST_CHANGES while a bot thread is open",
			fixture.state.lastSubmitReview["event"])
	}
}

// Publication is freed from the analysis deadline but not from shutdown.
//
// Analysis is what runs long, so its deadline must not cancel the publish that
// follows it, or a slow diff produces no review at all. Shutdown is different:
// a stopping service must not hold the process open writing to GitHub.
func TestServiceStopsPublicationWhenTheServiceShutsDown(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &sequenceModel{
			results: []domain.ReviewResult{{CoverageComplete: true, Findings: nil}},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	admitted, err := fixture.service.Admit(ctx, fixture.job())
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	cancel()

	if runErr := fixture.service.Run(ctx, admitted); runErr == nil {
		t.Fatal("Run: want the cancellation to stop the review")
	}
	if fixture.state.lastSubmitReview != nil {
		t.Fatal("a review was submitted after the service shut down")
	}
}

// A finding reaches the pull request as soon as its chunk answers, so the run
// stopping later cannot take it away.
//
// This is the whole point of streaming. Before, a review held every finding
// until the last chunk answered, so a provider refusal or an expired deadline
// left the reader with nothing even though most of the diff had been read.
func TestServiceStreamsEachFindingAsItsChunkAnswers(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		model: &sequenceModel{results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings: []domain.Finding{{
				Path:       "main.go",
				StartLine:  2,
				EndLine:    2,
				Title:      "Severe defect",
				Body:       "The changed line breaks core behavior.",
				Importance: 9,
			}},
		}}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fixture.state.streamedComments) != 1 {
		t.Fatalf("streamed comments = %d, want one", len(fixture.state.streamedComments))
	}
	// The comment posts before the review that carries the verdict, which is
	// what makes it survive a run that never reaches that submission.
	commentIndex := -1
	submitIndex := -1
	for index, route := range fixture.state.requestOrder {
		switch route {
		case "POST /repos/owner/repo/pulls/7/comments":
			commentIndex = index
		case "POST /repos/owner/repo/pulls/7/reviews":
			if submitIndex < 0 {
				submitIndex = index
			}
		}
	}
	if commentIndex < 0 || submitIndex < 0 || commentIndex > submitIndex {
		t.Fatalf("comment posted at %d and review submitted at %d, want the comment first",
			commentIndex, submitIndex)
	}
	submitted, ok := fixture.state.lastSubmitReview["comments"].([]any)
	if !ok || len(submitted) != 0 {
		t.Fatalf("submitted comments = %v, want none because they already posted",
			fixture.state.lastSubmitReview["comments"])
	}
}

// The check title is the one line a reader sees before opening anything, so it
// names the class of failure this service recognizes, and the stage when it
// recognizes none. It never quotes the provider: a check run is as public and
// as permanent as a comment, and a provider sentence can carry the request it
// failed on, an internal endpoint, or a credential.
//
// The causes below are the ones production actually produced, driven here
// through reconciliation, which is the stage that still fails a run outright
// and still makes a model call of its own. The two that state only a provider
// sentence fall back to the stage, and that sentence appears nowhere.
func TestCheckTitleNamesTheClassWithoutQuotingTheProvider(t *testing.T) {
	cases := []struct {
		name    string
		cause   error
		want    string
		leaking string
	}{
		{
			name:    "connection dropped",
			cause:   &stubProviderFailure{reason: "the connection carrying the answer closed early"},
			want:    "Review failed while reconciling existing findings.",
			leaking: "the connection carrying the answer closed early",
		},
		{
			name:    "provider refused the request",
			cause:   &stubProviderFailure{reason: "scan codex SSE events: upstream_malformed_request"},
			want:    "Review failed while reconciling existing findings.",
			leaking: "upstream_malformed_request",
		},
		{
			name:    "no remaining usage",
			cause:   &stubProviderFailure{reason: "The usage limit has been reached", usage: true},
			want:    "Review stopped: the model provider reported no remaining usage.",
			leaking: "The usage limit has been reached",
		},
		{
			name:    "ran out of time",
			cause:   fmt.Errorf("resolve threads: %w", context.DeadlineExceeded),
			want:    "Review stopped: it ran out of time.",
			leaking: "context deadline exceeded",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newServiceFixture(t, serviceFixtureOptions{
				reconcileErr: testCase.cause,
			})

			if err := fixture.run(context.Background(), fixture.job()); err == nil {
				t.Fatal("Run: want the reconciliation failure")
			}

			output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
			if !ok {
				t.Fatalf("output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
			}
			for _, surface := range []string{
				fmt.Sprint(output["title"]),
				fmt.Sprint(output["summary"]),
				fmt.Sprint(output["text"]),
				failureSummaryComment(t, fixture),
			} {
				if strings.Contains(surface, testCase.leaking) {
					t.Fatalf("a public surface published the provider's own words %q:\n%s",
						testCase.leaking, surface)
				}
			}
			if output["title"] != testCase.want {
				t.Fatalf("title = %v, want %q", output["title"], testCase.want)
			}
		})
	}
}

// stubProviderFailure states a cause the same way the model provider package
// does, without the review package importing it. Its sentence is the text no
// public surface may reprint.
type stubProviderFailure struct {
	reason string
	usage  bool
}

func (failure *stubProviderFailure) Error() string {
	return "model provider: " + failure.reason
}

func (failure *stubProviderFailure) UsageExceeded() bool {
	return failure.usage
}

// botReviewPage builds one page of existing reviews for the fixture server.
//
// The verdicts sit on an earlier head, which is the case that matters: it is
// the state a failed run used to reach in and change.
func botReviewPage(states ...string) [][]map[string]any {
	page := make([]map[string]any, 0, len(states))
	for index, state := range states {
		page = append(page, map[string]any{
			"id":        float64(500 + index),
			"commit_id": testStaleHeadSHA,
			"state":     state,
			"body":      "earlier verdict",
			"user":      map[string]any{"login": testBotLogin},
		})
	}
	return [][]map[string]any{page}
}

// A failed run knows nothing new about the head, so it touches no review
// object at all: it submits none, edits none, and withdraws none. It says why
// it stopped in the check run and in the one top level comment, which are the
// two places that carry no verdict.
//
// The earlier behavior withdrew every standing verdict here, which turned one
// provider outage into a pull request whose review history the service had
// silently rewritten.
func TestAFailedRunChangesNoReviewState(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reviewPages:  botReviewPage("APPROVED", "CHANGES_REQUESTED"),
		reconcileErr: errors.New("reconcile exploded"),
	})

	err := fixture.run(context.Background(), fixture.job())
	if err == nil || !strings.Contains(err.Error(), "reconcile exploded") {
		t.Fatalf("Run error = %v, want the failure surfaced", err)
	}

	if fixture.state.lastSubmitReview != nil {
		t.Fatalf("submitted review = %v, want none", fixture.state.lastSubmitReview)
	}
	if fixture.state.lastUpdateReview != nil {
		t.Fatalf("updated review = %v, want none", fixture.state.lastUpdateReview)
	}
	if len(fixture.state.dismissals) != 0 {
		t.Fatalf("dismissals = %v, want none", fixture.state.dismissals)
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf("conclusion = %v, want failure", fixture.state.lastUpdateCheckRun["conclusion"])
	}

	assertSanitizedFailureComment(
		t,
		fixture,
		"Review failed while reconciling existing findings.",
		"reconcile exploded",
	)
	if state := decodedSummaryState(t, fixture); state.Status != marker.StateFailed {
		t.Fatalf("state status = %q, want %q", state.Status, marker.StateFailed)
	}

	// The cause is not lost, only private: both public surfaces point at the
	// run identifier and neither reprints what the failure said.
	checkSummary, ok := checkOutput(t, fixture)["summary"].(string)
	if !ok {
		t.Fatalf("check summary = %v, want string", checkOutput(t, fixture)["summary"])
	}
	if strings.Contains(checkSummary, "reconcile exploded") {
		t.Fatalf("check summary published the raw cause:\n%s", checkSummary)
	}
	if !strings.Contains(checkSummary, "under run identifier `delivery-1`") {
		t.Fatalf("check summary lost the pointer to the cause:\n%s", checkSummary)
	}
}

// A run that changes no review state must also not move the checkpoint the last
// completed run recorded. Advancing it would skip a commit range nobody
// reviewed; erasing it would re-review everything already done.
func TestAFailedRunKeepsTheLastReviewedCommit(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reconcileErr: errors.New("reconcile exploded"),
	})
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id": float64(1),
		"body": "## Review\n\nan earlier run\n\n" + marker.EncodeState(marker.State{
			LastReviewed: domain.HeadSHA(testStaleHeadSHA),
			RunID:        "delivery-0",
			Status:       marker.StateDone,
			Pending:      nil,
		}),
		"user": map[string]any{"login": testBotLogin},
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want the reconciliation failure surfaced")
	}

	body, ok := fixture.state.issueComments[0]["body"].(string)
	if !ok {
		t.Fatalf("summary comment body = %v, want a string", fixture.state.issueComments[0]["body"])
	}
	state, ok := marker.DecodeState(body)
	if !ok {
		t.Fatalf("summary comment = %q, want a decodable state marker", body)
	}
	if state.LastReviewed != domain.HeadSHA(testStaleHeadSHA) {
		t.Fatalf("last reviewed = %q, want the earlier run's commit %q", state.LastReviewed, testStaleHeadSHA)
	}
	if state.Status != marker.StateFailed {
		t.Fatalf("state status = %q, want %q", state.Status, marker.StateFailed)
	}
}

// A run that leaves a chunk unread must not move the checkpoint either. The
// next run computes its delta from that commit, so advancing it would skip the
// very range this run could not read.
func TestAnIncompleteRunKeepsTheLastReviewedCommit(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &sequenceModel{err: errors.New("provider exploded")},
	})
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id": float64(1),
		"body": "## Review\n\nan earlier run\n\n" + marker.EncodeState(marker.State{
			LastReviewed: domain.HeadSHA(testStaleHeadSHA),
			RunID:        "delivery-0",
			Status:       marker.StateDone,
			Pending:      nil,
		}),
		"user": map[string]any{"login": testBotLogin},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	state := decodedSummaryState(t, fixture)
	if state.LastReviewed != domain.HeadSHA(testStaleHeadSHA) {
		t.Fatalf("last reviewed = %q, want the earlier run's commit %q", state.LastReviewed, testStaleHeadSHA)
	}
	if state.Status != marker.StateReviewing {
		t.Fatalf("state status = %q, want %q", state.Status, marker.StateReviewing)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("pending = %v, want the unread chunk", state.Pending)
	}
	if state.RunID != "delivery-1" {
		t.Fatalf("run id = %q, want this run's identifier", state.RunID)
	}
}

func TestServiceSerializesJobsForTheSamePullRequest(t *testing.T) {
	gateModel := newSerialGateModel()
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: gateModel,
	})

	job := fixture.job()
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)

	go func() {
		defer waitGroup.Done()
		if err := fixture.run(context.Background(), job); err != nil {
			t.Errorf("first Run: %v", err)
		}
	}()

	gateModel.waitUntilCalls(1)
	if gateModel.activeCount() != 1 {
		t.Fatalf("active model calls = %d, want 1", gateModel.activeCount())
	}

	go func() {
		defer waitGroup.Done()
		secondJob := domain.ReviewJob{
			DeliveryID: "delivery-2",
			PullRequestRef: domain.PullRequestRef{
				Repository:     job.Repository,
				Number:         job.Number,
				InstallationID: job.InstallationID,
				Head:           job.Head,
			},
		}
		if err := fixture.run(context.Background(), secondJob); err != nil {
			t.Errorf("second Run: %v", err)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	if gateModel.callCount() != 1 {
		t.Fatalf("model call count = %d, want 1 while first job holds lock", gateModel.callCount())
	}
	if gateModel.maxActive() != 1 {
		t.Fatalf("max active model calls = %d, want 1", gateModel.maxActive())
	}

	gateModel.release()
	gateModel.waitUntilCalls(2)
	gateModel.release()
	waitGroup.Wait()

	if gateModel.maxActive() != 1 {
		t.Fatalf("max active model calls = %d, want 1", gateModel.maxActive())
	}
}

type serviceFixtureOptions struct {
	reviewPages                     [][]map[string]any
	headAfterAnalysis               string
	pullRequestStatus               int
	pullRequestStatusAfterFirstRead int
	reviewListStatus                int
	submitReviewStatus              int
	updateReviewStatus              int
	createCommentStatus             int
	// createCommentHangup drops the connection instead of answering, which is
	// the failure a caller cannot tell apart from a comment that was created.
	createCommentHangup bool
	issueCommentStatus  int
	listThreadsStatus   int
	reconcileErr        error
	reconcileThreads    []githubapp.ReviewThread
	collector           review.Collector
	model               review.Model
	minimumImportance   int
	reviewMaxFiles      int
	reviewMaxChunks     int
	chunkTimeout        time.Duration
	// unsetReviewBudgets passes zero budgets to NewService, the way a caller
	// that never set them would.
	unsetReviewBudgets bool
	// logWriter receives the service's own log. It is discarded by default,
	// which is right for the tests that never read it and wrong for the one
	// invariant that is about what those lines carry: a sink that throws them
	// away lets any claim about them pass.
	logWriter io.Writer
}

type serviceFixture struct {
	service    *review.Service
	state      *serviceServerState
	reconciler *recordingReconciler
	model      review.Model
}

type serviceServerState struct {
	// mu serializes the fixture server. Chunks are reviewed several at a time,
	// so several requests reach this state at once and every field below would
	// otherwise be written concurrently.
	mu                              sync.Mutex
	requestOrder                    []string
	reviewPages                     [][]map[string]any
	reviewPageIndex                 int
	checkRuns                       []map[string]any
	nextCheckRunID                  int64
	headSHA                         string
	headAfterAnalysis               string
	pullRequestReads                int32
	pullRequestStatus               int
	pullRequestStatusAfterFirstRead int
	reviewListStatus                int
	lastSubmitReview                map[string]any
	// submittedReviews are the reviews this pull request now carries, which
	// ListReviews serves from the next call onward.
	submittedReviews    []map[string]any
	lastUpdateReview    map[string]any
	lastCreateCheckRun  map[string]any
	lastUpdateCheckRun  map[string]any
	checkDetailsURL     string
	submitReviewStatus  int
	updateReviewStatus  int
	dismissals          []map[string]any
	createCommentStatus int
	createCommentHangup bool
	issueCommentStatus  int
	listThreadsStatus   int
	streamedComments    []map[string]any
	// threadNodes is the GraphQL view of the pull request's review threads. A
	// posted inline comment opens one, exactly as GitHub does, which is what
	// makes the verdict's thread read see this run's own findings.
	threadNodes         []map[string]any
	issueComments       []map[string]any
	issueCommentUpdates int
}

// recordingReconciler answers with the configured threads plus one thread per
// inline comment the fixture server has accepted.
//
// That second half matters. In production the reconciler reads the pull
// request's real threads, so a comment an earlier run posted comes back as an
// open thread and suppresses the same finding on the next run. A double that
// returned only a fixed list would let a re-reviewed chunk post its finding
// twice and call it correct.
type recordingReconciler struct {
	callCount int
	threads   []githubapp.ReviewThread
	err       error
	state     *serviceServerState
}

func (reconciler *recordingReconciler) Reconcile(
	context.Context,
	domain.ReviewJob,
) ([]githubapp.ReviewThread, error) {
	reconciler.callCount++
	if reconciler.err != nil {
		return nil, reconciler.err
	}
	threads := append([]githubapp.ReviewThread{}, reconciler.threads...)
	return append(threads, reconciler.state.postedThreads()...), nil
}

// postedThreads renders every inline comment this pull request has received as
// the open thread GitHub would report for it.
func (state *serviceServerState) postedThreads() []githubapp.ReviewThread {
	state.mu.Lock()
	defer state.mu.Unlock()
	threads := make([]githubapp.ReviewThread, 0, len(state.streamedComments))
	for index, comment := range state.streamedComments {
		body, _ := comment["body"].(string)
		path, _ := comment["path"].(string)
		line, _ := comment["line"].(float64)
		startLine := line
		if start, ok := comment["start_line"].(float64); ok {
			startLine = start
		}
		threads = append(threads, githubapp.ReviewThread{
			NodeID:             fmt.Sprintf("posted-thread-%d", index+1),
			Resolved:           false,
			Outdated:           false,
			ViewerCanResolve:   true,
			ViewerCanUnresolve: true,
			RootComment: domain.ReviewComment{
				DatabaseID: int64(900 + index + 1),
				Author:     testBotLogin,
				Body:       body,
				Path:       path,
				StartLine:  int(startLine),
				EndLine:    int(line),
			},
		})
	}
	return threads
}

type stubCollector struct{}

func (stubCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	patch := strings.Join([]string{
		"@@ -1,1 +1,4 @@",
		" package main",
		"+added",
		"+second",
		"+third",
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
			CurrentContent:    "package main\nadded\nsecond\nthird\n",
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		}},
	}, nil
}

// recordingDeltaCollector records the base every CollectRange call received,
// so a test can prove what the service actually asked the collector for
// rather than only that a run completed.
type recordingDeltaCollector struct {
	mu         sync.Mutex
	rangeBases []domain.HeadSHA
}

func (collector *recordingDeltaCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	base domain.HeadSHA,
) (diff.ReviewInput, error) {
	collector.mu.Lock()
	collector.rangeBases = append(collector.rangeBases, base)
	collector.mu.Unlock()

	patch := "@@ -1,1 +1,2 @@\n package main\n+added\n"
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
			CurrentContent:    "package main\nadded\n",
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		}},
	}, nil
}

func (collector *recordingDeltaCollector) bases() []domain.HeadSHA {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]domain.HeadSHA{}, collector.rangeBases...)
}

// twoChunkCollector returns a diff large enough that the chunking path always
// used to measure and to build the model prompt produces two chunks, so an
// admission gate configured for one chunk refuses it.
type twoChunkCollector struct{}

func (twoChunkCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	padding := strings.Repeat("x\n", 30000)
	files := make([]diff.FileContext, 0, 2)
	for index := range 2 {
		patch := fmt.Sprintf("@@ -1,1 +1,2 @@\n line%d\n+added%d\n", index, index)
		changed, hunks, err := diff.ChangedRightLines(patch)
		if err != nil {
			return diff.ReviewInput{}, err
		}
		files = append(files, diff.FileContext{
			Path:              fmt.Sprintf("file%d.go", index),
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    padding,
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		})
	}
	return diff.ReviewInput{PullRequest: pullRequest, Files: files}, nil
}

type serialGateModel struct {
	mu      sync.Mutex
	active  int
	maxSeen int
	calls   int
	gate    chan struct{}
}

func newSerialGateModel() *serialGateModel {
	return &serialGateModel{
		gate: make(chan struct{}, 2),
	}
}

func (model *serialGateModel) Review(context.Context, string) (review.Completion, error) {
	model.mu.Lock()
	model.active++
	model.calls++
	if model.active > model.maxSeen {
		model.maxSeen = model.active
	}
	model.mu.Unlock()

	<-model.gate

	model.mu.Lock()
	model.active--
	model.mu.Unlock()

	return review.Completion{
		Result: domain.ReviewResult{CoverageComplete: true},
		Model:  testReviewModel,
	}, nil
}

func (model *serialGateModel) waitUntilCalls(count int) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if model.callCount() >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (model *serialGateModel) release() {
	model.gate <- struct{}{}
}

func (model *serialGateModel) activeCount() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.active
}

func (model *serialGateModel) maxActive() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.maxSeen
}

func (model *serialGateModel) callCount() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.calls
}

func newServiceFixture(t *testing.T, options serviceFixtureOptions) *serviceFixture {
	t.Helper()

	privateKey := serviceTestPrivateKey(t)
	state := &serviceServerState{
		reviewPages:                     [][]map[string]any{{}},
		checkRuns:                       []map[string]any{},
		issueComments:                   []map[string]any{},
		nextCheckRunID:                  77,
		headSHA:                         testHeadSHA,
		headAfterAnalysis:               options.headAfterAnalysis,
		pullRequestStatus:               options.pullRequestStatus,
		pullRequestStatusAfterFirstRead: options.pullRequestStatusAfterFirstRead,
		reviewListStatus:                options.reviewListStatus,
		submitReviewStatus:              options.submitReviewStatus,
		updateReviewStatus:              options.updateReviewStatus,
		createCommentStatus:             options.createCommentStatus,
		createCommentHangup:             options.createCommentHangup,
		issueCommentStatus:              options.issueCommentStatus,
		listThreadsStatus:               options.listThreadsStatus,
		threadNodes:                     threadNodesFor(options.reconcileThreads),
	}
	if len(options.reviewPages) > 0 {
		state.reviewPages = options.reviewPages
	}
	if state.submitReviewStatus == 0 {
		state.submitReviewStatus = http.StatusOK
	}
	if state.updateReviewStatus == 0 {
		state.updateReviewStatus = http.StatusOK
	}
	if state.createCommentStatus == 0 {
		state.createCommentStatus = http.StatusOK
	}
	if state.issueCommentStatus == 0 {
		state.issueCommentStatus = http.StatusOK
	}
	if state.listThreadsStatus == 0 {
		state.listThreadsStatus = http.StatusOK
	}
	if state.pullRequestStatus == 0 {
		state.pullRequestStatus = http.StatusOK
	}
	if state.reviewListStatus == 0 {
		state.reviewListStatus = http.StatusOK
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		if strings.Contains(request.URL.Path, "/pulls/comments/") && strings.HasSuffix(request.URL.Path, "/replies") {
			http.Error(writer, "replies forbidden", http.StatusForbidden)
			return
		}
		handleServiceRequest(writer, request, state)
	}))
	t.Cleanup(server.Close)

	apiURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse api URL: %v", err)
	}
	graphqlURL, err := url.Parse(server.URL + "/graphql")
	if err != nil {
		t.Fatalf("Parse graphql URL: %v", err)
	}

	cfg := config.Config{
		Port:             "3000",
		GitHubAppID:      12345,
		GitHubPrivateKey: privateKey, // gitleaks:allow
		GitHubBotLogin:   testBotLogin,
		GitHubAPIBaseURL: apiURL,
		GitHubGraphQLURL: graphqlURL,
	}
	client := githubapp.NewClient(
		cfg,
		server.Client(),
		func() time.Time { return time.Unix(1_700_000_000, 0) },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	reconciler := &recordingReconciler{
		threads: options.reconcileThreads,
		err:     options.reconcileErr,
		state:   state,
	}
	collector := options.collector
	if collector == nil {
		collector = stubCollector{}
	}
	model := options.model
	minimumImportance := options.minimumImportance
	if minimumImportance == 0 {
		minimumImportance = testMinimumImportance
	}
	chunkTimeout := options.chunkTimeout
	if chunkTimeout == 0 {
		chunkTimeout = config.DefaultReviewChunkTimeout
	}
	reviewMaxFiles := options.reviewMaxFiles
	if reviewMaxFiles == 0 {
		reviewMaxFiles = config.DefaultReviewMaxFiles
	}
	reviewMaxChunks := options.reviewMaxChunks
	if reviewMaxChunks == 0 {
		reviewMaxChunks = config.DefaultReviewMaxChunks
	}
	if options.unsetReviewBudgets {
		reviewMaxFiles = 0
		reviewMaxChunks = 0
	}
	if model == nil {
		model = &sequenceModel{
			results: []domain.ReviewResult{{
				CoverageComplete: true,
				Findings: []domain.Finding{{
					Path:       "main.go",
					StartLine:  2,
					EndLine:    2,
					Title:      "Severe defect",
					Body:       "The changed line breaks core behavior.",
					Importance: testMinimumImportance,
				}},
			}},
		}
	}

	logWriter := options.logWriter
	if logWriter == nil {
		logWriter = io.Discard
	}

	service := review.NewService(
		client,
		collector,
		model,
		reconciler,
		queue.NewKeyedLocker(),
		testBotLogin,
		minimumImportance,
		reviewMaxFiles,
		reviewMaxChunks,
		chunkTimeout,
		testClock(8*time.Second),
		slog.New(slog.NewTextHandler(logWriter, nil)),
	)

	return &serviceFixture{
		service:    service,
		state:      state,
		reconciler: reconciler,
		model:      model,
	}
}

func (fixture *serviceFixture) run(ctx context.Context, job domain.ReviewJob) error {
	admitted, err := fixture.service.Admit(ctx, job)
	if err != nil {
		return err
	}
	return fixture.service.Run(ctx, admitted)
}

func (fixture *serviceFixture) job() domain.ReviewJob {
	return domain.ReviewJob{
		DeliveryID: "delivery-1",
		PullRequestRef: domain.PullRequestRef{
			Repository:     domain.Repository{Owner: "owner", Name: "repo"},
			Number:         testPRNumber,
			InstallationID: 99,
			Head:           domain.HeadSHA(testHeadSHA),
		},
	}
}

func handleServiceRequest(writer http.ResponseWriter, request *http.Request, state *serviceServerState) {
	route := fmt.Sprintf("%s %s", request.Method, request.URL.Path)
	if !strings.HasSuffix(request.URL.Path, "/access_tokens") {
		state.requestOrder = append(state.requestOrder, route)
	}

	if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/access_tokens") {
		serviceWriteJSON(writer, http.StatusCreated, map[string]any{
			"token":      "ghs_installation",
			"expires_at": time.Unix(1_700_000_600, 0).UTC().Format(time.RFC3339),
		})
		return
	}

	if request.Method == http.MethodPost && request.URL.Path == "/graphql" {
		writeServiceThreadPage(writer, state)
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/") && strings.HasSuffix(request.URL.Path, "/reviews") {
		writeServiceReviewPage(writer, request, state)
		return
	}

	// The service's one top level comment lives under the issue comments
	// endpoint, distinct from the inline review comments matched below.
	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/issues/") && strings.HasSuffix(request.URL.Path, "/comments") {
		if state.issueCommentStatus != http.StatusOK {
			serviceWriteJSON(writer, state.issueCommentStatus, map[string]any{"message": "list issue comments failed"})
			return
		}
		serviceWriteJSON(writer, http.StatusOK, state.issueComments)
		return
	}

	if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/issues/") && strings.HasSuffix(request.URL.Path, "/comments") {
		if state.issueCommentStatus != http.StatusOK {
			serviceWriteJSON(writer, state.issueCommentStatus, map[string]any{"message": "create issue comment failed"})
			return
		}
		body, err := serviceReadJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		created := map[string]any{
			"id":   float64(2000 + len(state.issueComments)),
			"body": body["body"],
			"user": map[string]any{"login": testBotLogin},
		}
		state.issueComments = append(state.issueComments, created)
		serviceWriteJSON(writer, http.StatusCreated, created)
		return
	}

	if request.Method == http.MethodPatch && strings.Contains(request.URL.Path, "/issues/comments/") {
		if state.issueCommentStatus != http.StatusOK {
			serviceWriteJSON(writer, state.issueCommentStatus, map[string]any{"message": "update issue comment failed"})
			return
		}
		body, err := serviceReadJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		idText := strings.TrimPrefix(request.URL.Path, "/repos/owner/repo/issues/comments/")
		var updated map[string]any
		for index, item := range state.issueComments {
			if fmt.Sprintf("%.0f", item["id"]) != idText {
				continue
			}
			item["body"] = body["body"]
			state.issueComments[index] = item
			updated = item
		}
		state.issueCommentUpdates++
		serviceWriteJSON(writer, http.StatusOK, updated)
		return
	}

	// Findings post one at a time as their chunks answer, so this endpoint
	// carries every inline comment a review produces.
	if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/comments") {
		// A dropped connection, distinct from an answer GitHub sent. The client
		// cannot tell whether the comment was created, so the caller must treat
		// it as unfinished rather than as a refusal.
		if state.createCommentHangup {
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				http.Error(writer, "cannot hijack", http.StatusInternalServerError)
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				http.Error(writer, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = connection.Close()
			return
		}
		if state.createCommentStatus != http.StatusOK {
			serviceWriteJSON(writer, state.createCommentStatus, map[string]any{
				"message": "create review comment failed",
			})
			return
		}
		body, err := serviceReadJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		state.streamedComments = append(state.streamedComments, body)
		// A posted comment opens a review thread, which is one of the two
		// inputs the verdict is computed from.
		state.threadNodes = append(state.threadNodes, map[string]any{
			"id":         fmt.Sprintf("streamed-thread-%d", len(state.streamedComments)),
			"isResolved": false,
			"comments": map[string]any{"nodes": []map[string]any{{
				"databaseId": float64(900 + len(state.streamedComments)),
				"body":       body["body"],
				"path":       body["path"],
				"line":       body["line"],
				"startLine":  body["start_line"],
				"author":     map[string]any{"login": testBotLogin},
			}}},
		})
		serviceWriteJSON(writer, http.StatusCreated, map[string]any{
			"id":   float64(900 + len(state.streamedComments)),
			"body": body["body"],
			"user": map[string]any{"login": testBotLogin},
		})
		return
	}

	if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/pulls/") && strings.HasSuffix(request.URL.Path, "/reviews") {
		if state.submitReviewStatus != http.StatusOK {
			serviceWriteJSON(writer, state.submitReviewStatus, map[string]any{
				"message": "submit review failed",
			})
			return
		}
		body, err := serviceReadJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		state.lastSubmitReview = body
		// A submitted review is one GitHub lists from then on, which is how a
		// later run finds the verdict an earlier one left. A fixture that kept
		// them out of ListReviews would let a run leave a second standing
		// review and call it one.
		created := map[string]any{
			"id":        float64(4200 + len(state.submittedReviews)),
			"commit_id": body["commit_id"],
			"state":     "COMMENTED",
			"body":      body["body"],
			"user":      map[string]any{"login": testBotLogin},
		}
		state.submittedReviews = append(state.submittedReviews, created)
		serviceWriteJSON(writer, http.StatusOK, created)
		return
	}

	// Nothing in the service dismisses a review any more. The route stays so a
	// test can prove that: an attempt would be recorded here rather than
	// answered with a 404 nobody sees.
	if request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/dismissals") {
		body, err := serviceReadJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		trimmed := strings.TrimSuffix(request.URL.Path, "/dismissals")
		body["review_id"] = trimmed[strings.LastIndex(trimmed, "/")+1:]
		state.dismissals = append(state.dismissals, body)
		serviceWriteJSON(writer, http.StatusOK, map[string]any{
			"id":        float64(42),
			"commit_id": testHeadSHA,
			"state":     "DISMISSED",
			"body":      "",
			"user":      map[string]any{"login": testBotLogin},
		})
		return
	}

	if request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/pulls/") && strings.Contains(request.URL.Path, "/reviews/") {
		if state.updateReviewStatus != http.StatusOK {
			serviceWriteJSON(writer, state.updateReviewStatus, map[string]any{
				"message": "update review failed",
			})
			return
		}
		body, err := serviceReadJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		state.lastUpdateReview = body
		serviceWriteJSON(writer, http.StatusOK, map[string]any{
			"id":        float64(42),
			"commit_id": testHeadSHA,
			"state":     "CHANGES_REQUESTED",
			"body":      body["body"],
			"user":      map[string]any{"login": testBotLogin},
		})
		return
	}

	if request.Method == http.MethodGet &&
		strings.Contains(request.URL.Path, "/commits/") &&
		strings.HasSuffix(request.URL.Path, "/check-runs") {
		query := request.URL.Query()
		pathWithoutSuffix := strings.TrimSuffix(request.URL.Path, "/check-runs")
		headSHA := pathWithoutSuffix[strings.LastIndex(pathWithoutSuffix, "/")+1:]
		checkName := query.Get("check_name")
		matches := make([]map[string]any, 0)
		for _, item := range state.checkRuns {
			if item["head_sha"] == headSHA && item["name"] == checkName {
				matches = append(matches, item)
			}
		}
		serviceWriteJSON(writer, http.StatusOK, map[string]any{
			"total_count": len(matches),
			"check_runs":  matches,
		})
		return
	}

	if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/check-runs") {
		body, err := serviceReadJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		state.lastCreateCheckRun = body
		created := map[string]any{
			"id":         float64(state.nextCheckRunID),
			"name":       body["name"],
			"head_sha":   body["head_sha"],
			"status":     body["status"],
			"conclusion": "",
		}
		state.checkRuns = append(state.checkRuns, created)
		serviceWriteJSON(writer, http.StatusCreated, created)
		return
	}

	if request.Method == http.MethodPatch && strings.Contains(request.URL.Path, "/check-runs/") {
		body, err := serviceReadJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		state.lastUpdateCheckRun = body
		if body["status"] == "in_progress" {
			state.checkDetailsURL, _ = body["details_url"].(string)
		}
		checkIDText := strings.TrimPrefix(request.URL.Path, "/repos/owner/repo/check-runs/")
		for index, item := range state.checkRuns {
			if fmt.Sprintf("%.0f", item["id"]) != checkIDText {
				continue
			}
			if status, ok := body["status"].(string); ok && status != "" {
				item["status"] = status
			}
			if conclusion, ok := body["conclusion"].(string); ok {
				item["conclusion"] = conclusion
			}
			state.checkRuns[index] = item
		}
		serviceWriteJSON(writer, http.StatusOK, map[string]any{
			"id":         float64(state.nextCheckRunID),
			"name":       config.ReviewCheckName,
			"head_sha":   state.headSHA,
			"status":     body["status"],
			"conclusion": body["conclusion"],
		})
		return
	}

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/") {
		if state.pullRequestStatus != http.StatusOK {
			serviceWriteJSON(writer, state.pullRequestStatus, map[string]any{"message": "pull request load failed"})
			return
		}
		readCount := atomic.AddInt32(&state.pullRequestReads, 1)
		if readCount > 1 && state.pullRequestStatusAfterFirstRead != 0 {
			serviceWriteJSON(writer, state.pullRequestStatusAfterFirstRead, map[string]any{
				"message": "pull request refresh failed",
			})
			return
		}
		head := state.headSHA
		if readCount > 1 && state.headAfterAnalysis != "" {
			head = state.headAfterAnalysis
		}
		serviceWriteJSON(writer, http.StatusOK, map[string]any{
			"number": float64(testPRNumber),
			"draft":  false,
			"title":  "title",
			"body":   "body",
			"head":   map[string]any{"sha": head},
			"base":   map[string]any{"sha": testBaseSHA},
		})
		return
	}

	writer.WriteHeader(http.StatusNotFound)
}

// threadNodesFor renders configured threads the way the GraphQL API returns
// them, so the service reads its own threads through the real client rather
// than through a hand-built double.
func threadNodesFor(threads []githubapp.ReviewThread) []map[string]any {
	nodes := make([]map[string]any, 0, len(threads))
	for _, thread := range threads {
		nodes = append(nodes, map[string]any{
			"id":                 thread.NodeID,
			"isResolved":         thread.Resolved,
			"isOutdated":         thread.Outdated,
			"viewerCanResolve":   thread.ViewerCanResolve,
			"viewerCanUnresolve": thread.ViewerCanUnresolve,
			"comments": map[string]any{"nodes": []map[string]any{{
				"databaseId": float64(thread.RootComment.DatabaseID),
				"body":       thread.RootComment.Body,
				"path":       thread.RootComment.Path,
				"line":       float64(thread.RootComment.EndLine),
				"startLine":  float64(thread.RootComment.StartLine),
				"author":     map[string]any{"login": thread.RootComment.Author},
			}}},
		})
	}
	return nodes
}

func writeServiceThreadPage(writer http.ResponseWriter, state *serviceServerState) {
	if state.listThreadsStatus != http.StatusOK {
		serviceWriteJSON(writer, state.listThreadsStatus, map[string]any{"message": "list review threads failed"})
		return
	}
	serviceWriteJSON(writer, http.StatusOK, map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"pullRequest": map[string]any{
					"reviewThreads": map[string]any{
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						"nodes":    state.threadNodes,
					},
				},
			},
		},
	})
}

func writeServiceReviewPage(writer http.ResponseWriter, request *http.Request, state *serviceServerState) {
	if state.reviewListStatus != http.StatusOK {
		serviceWriteJSON(writer, state.reviewListStatus, map[string]any{"message": "list reviews failed"})
		return
	}
	// GitHub returns the same reviews to every caller, so replay the page set
	// from the start once a prior read has consumed it.
	if state.reviewPageIndex >= len(state.reviewPages) {
		state.reviewPageIndex = 0
	}
	page := state.reviewPages[state.reviewPageIndex]
	state.reviewPageIndex++
	if state.reviewPageIndex == len(state.reviewPages) {
		page = append(append([]map[string]any{}, page...), state.submittedReviews...)
	}
	if state.reviewPageIndex < len(state.reviewPages) {
		next := fmt.Sprintf("http://%s%s", request.Host, request.URL.Path)
		if request.URL.RawQuery != "" {
			next += "?" + request.URL.RawQuery + "&page=2"
		} else {
			next += "?page=2"
		}
		writer.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
	}
	serviceWriteJSON(writer, http.StatusOK, page)
}

func assertRequestOrder(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("request count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("request[%d] = %q, want %q\nfull got:  %v\nfull want: %v", index, got[index], want[index], got, want)
		}
	}
}

func serviceTestPrivateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return privateKey
}

func serviceWriteJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func serviceReadJSONBody(request *http.Request) (map[string]any, error) {
	defer func() {
		_ = request.Body.Close()
	}()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

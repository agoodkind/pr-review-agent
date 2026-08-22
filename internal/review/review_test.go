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
// deterministic without any sleeping.
func testClock(step time.Duration) func() time.Time {
	current := time.Unix(1_700_000_000, 0).UTC()
	return func() time.Time {
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

func TestRenderFailureBodyFencesTheCauseAndOmitsTheReviewMarker(t *testing.T) {
	t.Run("plain cause", func(t *testing.T) {
		summary := testSummary()
		body := review.RenderFailureBody(summary, "Review failed.", "provider refused")
		summary.Failed = true
		want := "## Review\n\nReview failed.\n\n```\nprovider refused\n```\n\n" +
			review.RenderDetails(summary) + "\n\n" + marker.Summary()
		if body != want {
			t.Fatalf("body = %q, want %q", body, want)
		}
	})

	t.Run("cause containing a fence", func(t *testing.T) {
		detail := "provider said ``` and then ````"
		body := review.RenderFailureBody(testSummary(), "Review failed.", detail)
		if !strings.Contains(body, "`````\n"+detail+"\n`````") {
			t.Fatalf("body = %q, want a fence longer than the cause backtick run", body)
		}
	})

	t.Run("no cause", func(t *testing.T) {
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

// multiHunkInput builds one file whose patch has four hunks, so its chunk can
// split twice before reaching the one hunk floor.
func multiHunkInput(t *testing.T) diff.ReviewInput {
	t.Helper()
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
		t.Fatalf("ChangedRightLines: %v", err)
	}
	return diff.ReviewInput{
		PullRequest: githubapp.PullRequest{Head: domain.HeadSHA(testHeadSHA)},
		Files: []diff.FileContext{{
			Path:              "main.go",
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    "package main\nadded1\nadded2\nadded3\nadded4\n",
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		}},
	}
}

func TestAnalyzeSplitsAndRetriesATruncatedChunk(t *testing.T) {
	input := multiHunkInput(t)
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

	analysis, err := review.Analyze(context.Background(), model, input, 9)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if model.calls != 3 {
		t.Fatalf("model calls = %d, want 3: one truncated, then both halves", model.calls)
	}
	if len(analysis.Anchored) != 1 {
		t.Fatalf("anchored = %d, want the finding recovered from the halves", len(analysis.Anchored))
	}
	if !analysis.CoverageComplete {
		t.Fatal("CoverageComplete = false, want true because every hunk was reviewed")
	}
}

func TestAnalyzeKeepsSplittingWhileTheAnswerTruncates(t *testing.T) {
	input := multiHunkInput(t)
	model := &truncatedModel{truncateCalls: 2}

	analysis, err := review.Analyze(context.Background(), model, input, 9)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	// Four hunks: the whole chunk truncates, its first half truncates again,
	// then both single hunks answer, then the second half answers.
	if model.calls != 5 {
		t.Fatalf("model calls = %d, want 5 as the split recurses twice", model.calls)
	}
	if !analysis.CoverageComplete {
		t.Fatal("CoverageComplete = false, want true because every hunk was eventually reviewed")
	}
}

func TestAnalyzeSkipsAHunkThatCannotSplitAndReportsIncompleteCoverage(t *testing.T) {
	input := multiHunkInput(t)
	model := &truncatedModel{truncateCalls: 1000}

	analysis, err := review.Analyze(context.Background(), model, input, 9)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if analysis.CoverageComplete {
		t.Fatal("CoverageComplete = true, want false after skipping unsplittable hunks")
	}
	if len(analysis.Observed) != 0 {
		t.Fatalf("observed = %d, want none", len(analysis.Observed))
	}
}

func TestAnalyzeFailsOnAnErrorThatSplittingCannotFix(t *testing.T) {
	input := multiHunkInput(t)
	model := &sequenceModel{err: errors.New("provider refused the prompt")}

	_, err := review.Analyze(context.Background(), model, input, 9)
	if err == nil {
		t.Fatal("Analyze: want the provider error")
	}
	if !strings.Contains(err.Error(), "provider refused the prompt") {
		t.Fatalf("err = %q, want the provider cause", err)
	}
	if len(model.prompts) != 1 {
		t.Fatalf("model calls = %d, want 1 because splitting cannot fix a refusal", len(model.prompts))
	}
}

func TestAnalyzeAggregatesChunksDedupesFindingsAndClassifiesBadAnchors(t *testing.T) {
	largeContent := strings.Repeat("x\n", 20000)
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
		t.Fatalf("ChangedRightLines: %v", err)
	}

	input := diff.ReviewInput{
		PullRequest: githubapp.PullRequest{Head: domain.HeadSHA(testHeadSHA)},
		Files: []diff.FileContext{
			{
				Path:              "main.go",
				Status:            "modified",
				Patch:             patch,
				CurrentContent:    largeContent,
				ChangedRightLines: changed,
				ChangedRightHunks: hunks,
				CoverageComplete:  true,
			},
			{
				Path:             "binary.bin",
				Status:           "binary",
				Patch:            "Binary files differ",
				CoverageComplete: false,
			},
		},
	}

	chunks, err := diff.ChunkInput(input, config.MaximumPromptBytes)
	if err != nil {
		t.Fatalf("ChunkInput: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("chunk count = %d, want at least 2", len(chunks))
	}

	model := &sequenceModel{
		results: []domain.ReviewResult{
			{
				CoverageComplete: true,
				Findings: []domain.Finding{{
					Path:       "main.go",
					StartLine:  2,
					EndLine:    2,
					Title:      "Duplicate",
					Body:       "Same finding.",
					Importance: 9,
				}},
			},
			{
				CoverageComplete: true,
				Findings: []domain.Finding{
					{
						Path:       "main.go",
						StartLine:  2,
						EndLine:    2,
						Title:      "Duplicate",
						Body:       "Same finding.",
						Importance: 9,
					},
					{
						Path:       "main.go",
						StartLine:  99,
						EndLine:    99,
						Title:      "Bad anchor",
						Body:       "Line is outside the diff.",
						Importance: 3,
					},
					{
						Path:       "../escape.go",
						StartLine:  1,
						EndLine:    1,
						Title:      "Bad path",
						Body:       "Path normalizes to traversal.",
						Importance: 2,
					},
				},
			},
		},
	}

	analysis, err := review.Analyze(context.Background(), model, input, 9)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if model.callCount != len(chunks) {
		t.Fatalf("model call count = %d, want %d", model.callCount, len(chunks))
	}
	if analysis.CoverageComplete {
		t.Fatal("coverage complete = true, want false from binary file")
	}
	if len(analysis.Anchored) != 1 {
		t.Fatalf("anchored count = %d, want 1", len(analysis.Anchored))
	}
	if analysis.Decision != domain.ReviewDecisionRequestChanges {
		t.Fatalf("decision = %q, want %q", analysis.Decision, domain.ReviewDecisionRequestChanges)
	}
}

func TestAnalyzePromptClassifiesFindingsAndWrapsUntrustedInput(t *testing.T) {
	patch := strings.Join([]string{
		"@@ -1,1 +1,2 @@",
		" package main",
		"+added",
	}, "\n")
	changed, hunks, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatalf("ChangedRightLines: %v", err)
	}

	input := diff.ReviewInput{
		Files: []diff.FileContext{{
			Path:              "main.go",
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    "package main\nadded\n",
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		}},
	}

	model := &sequenceModel{
		results: []domain.ReviewResult{{
			CoverageComplete: true,
		}},
	}

	_, err = review.Analyze(context.Background(), model, input, 9)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(model.prompts) != 1 {
		t.Fatalf("prompt count = %d, want 1", len(model.prompts))
	}
	prompt := model.prompts[0]
	if !strings.Contains(prompt, "importance 9 or higher") {
		t.Fatalf("prompt missing configured importance: %q", prompt)
	}
	if !strings.Contains(prompt, "backticks") {
		t.Fatalf("prompt missing code formatting rule: %q", prompt)
	}
	if !strings.Contains(prompt, "exact replacement") {
		t.Fatalf("prompt missing suggestion rule: %q", prompt)
	}
	if !strings.Contains(prompt, "<<<UNTRUSTED_INPUT>>>") {
		t.Fatalf("prompt missing untrusted input delimiter: %q", prompt)
	}
}

func TestAnalyzeFailsOnInvalidModelResult(t *testing.T) {
	patch := strings.Join([]string{
		"@@ -1,1 +1,2 @@",
		" package main",
		"+added",
	}, "\n")
	changed, hunks, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatalf("ChangedRightLines: %v", err)
	}

	input := diff.ReviewInput{
		Files: []diff.FileContext{{
			Path:              "main.go",
			Status:            "modified",
			Patch:             patch,
			CurrentContent:    "package main\nadded\n",
			ChangedRightLines: changed,
			ChangedRightHunks: hunks,
			CoverageComplete:  true,
		}},
	}

	model := &sequenceModel{
		results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings: []domain.Finding{{
				Path:       "main.go",
				StartLine:  2,
				EndLine:    2,
				Title:      "",
				Body:       "Invalid title.",
				Importance: 5,
			}},
		}},
	}

	_, err = review.Analyze(context.Background(), model, input, 9)
	if err == nil {
		t.Fatal("Analyze invalid model result: want error")
	}
}

type sequenceModel struct {
	results   []domain.ReviewResult
	models    []string
	prompts   []string
	callCount int
	err       error
}

func (model *sequenceModel) Review(_ context.Context, prompt string) (review.Completion, error) {
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

	wantOrder := []string{
		"GET /repos/owner/repo/commits/a3c4f1cac7f595bc824704b9d2a1f1191630dc32/check-runs",
		"POST /repos/owner/repo/check-runs",
		"PATCH /repos/owner/repo/check-runs/77",
		"GET /repos/owner/repo/pulls/7",
		"GET /repos/owner/repo/pulls/7/reviews",
		"GET /repos/owner/repo/pulls/7",
		"POST /repos/owner/repo/pulls/7/reviews",
		"PATCH /repos/owner/repo/check-runs/77",
	}
	assertRequestOrder(t, fixture.state.requestOrder, wantOrder)

	if fixture.state.lastSubmitReview == nil {
		t.Fatal("SubmitReview was not called")
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "success" {
		t.Fatalf("conclusion = %v, want success", fixture.state.lastUpdateCheckRun["conclusion"])
	}
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
		"GET /repos/owner/repo/pulls/7",
		"POST /repos/owner/repo/pulls/7/reviews",
		"PATCH /repos/owner/repo/check-runs/77",
	}
	assertRequestOrder(t, fixture.state.requestOrder, wantOrder)
	if fixture.state.lastSubmitReview == nil {
		t.Fatal("SubmitReview was not called")
	}
}

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

	wantOrder := []string{
		"GET /repos/owner/repo/commits/a3c4f1cac7f595bc824704b9d2a1f1191630dc32/check-runs",
		"POST /repos/owner/repo/check-runs",
		"PATCH /repos/owner/repo/check-runs/77",
		"GET /repos/owner/repo/pulls/7",
		"GET /repos/owner/repo/pulls/7/reviews",
		"GET /repos/owner/repo/pulls/7",
		"PATCH /repos/owner/repo/check-runs/77",
	}
	assertRequestOrder(t, fixture.state.requestOrder, wantOrder)
	if fixture.state.lastSubmitReview != nil {
		t.Fatal("SubmitReview was called after stale head")
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

	// The existing reviews are read once before the check completes, so any
	// approval that outlived the failure can be withdrawn and named in the
	// check rather than discovered afterwards.
	wantOrder := []string{
		"GET /repos/owner/repo/commits/a3c4f1cac7f595bc824704b9d2a1f1191630dc32/check-runs",
		"POST /repos/owner/repo/check-runs",
		"PATCH /repos/owner/repo/check-runs/77",
		"GET /repos/owner/repo/pulls/7",
		"GET /repos/owner/repo/pulls/7/reviews",
		"GET /repos/owner/repo/pulls/7",
		"POST /repos/owner/repo/pulls/7/reviews",
		"GET /repos/owner/repo/pulls/7/reviews",
		"PATCH /repos/owner/repo/check-runs/77",
		"POST /repos/owner/repo/pulls/7/reviews",
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
	notice := fixture.state.lastSubmitReview
	if notice == nil {
		t.Fatal("failure notice was not published")
	}
	if notice["event"] != string(domain.ReviewDecisionComment) {
		t.Fatalf("event = %v, want COMMENT", notice["event"])
	}
	comments, ok := notice["comments"].([]any)
	if !ok || len(comments) != 0 {
		t.Fatalf("comments = %v, want none on a failure notice", notice["comments"])
	}
}

func TestServiceReportsModelFailureCause(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &sequenceModel{err: errors.New("provider rejected response schema")},
	})

	err := fixture.run(context.Background(), fixture.job())
	if err == nil {
		t.Fatal("Run: want error")
	}
	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	if output["title"] != "Review failed during model analysis." {
		t.Fatalf("title = %v, want model analysis stage", output["title"])
	}
	summary, ok := output["summary"].(string)
	if !ok || !strings.Contains(summary, "provider rejected response schema") {
		t.Fatalf("summary = %v, want provider failure", output["summary"])
	}
}

func TestServiceNamesUsageExhaustionInCheckAndNotice(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &sequenceModel{err: quotaExhaustedError{}},
	})

	err := fixture.run(context.Background(), fixture.job())
	if err == nil {
		t.Fatal("Run: want error")
	}

	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	wantTitle := "Review stopped: the model provider reported no remaining usage."
	if output["title"] != wantTitle {
		t.Fatalf("title = %v, want %q", output["title"], wantTitle)
	}

	notice := fixture.state.lastSubmitReview
	if notice == nil {
		t.Fatal("failure notice was not published")
	}
	body, ok := notice["body"].(string)
	if !ok || !strings.Contains(body, wantTitle) {
		t.Fatalf("notice body = %v, want the usage wording", notice["body"])
	}
	if !strings.Contains(body, "usage credits are exhausted") {
		t.Fatalf("notice body = %q, want the provider detail", body)
	}
}

func TestServiceFailureNoticeOmitsReviewMarkerSoTheHeadIsReviewedAgain(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &sequenceModel{err: errors.New("provider rejected response schema")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want error")
	}
	notice := fixture.state.lastSubmitReview
	if notice == nil {
		t.Fatal("failure notice was not published")
	}
	body, ok := notice["body"].(string)
	if !ok {
		t.Fatalf("notice body = %v, want string", notice["body"])
	}
	if !marker.HasSummary(body) {
		t.Fatalf("notice body = %q, want the summary marker so a later run edits it", body)
	}
	if _, found := marker.FindReview(body); found {
		t.Fatalf("notice body = %q, want no review marker", body)
	}
}

func TestServiceFailureNoticeEditsTheExistingSummary(t *testing.T) {
	summaryReview := map[string]any{
		"id":        float64(42),
		"commit_id": testStaleHeadSHA,
		"body":      "## Review\n\nNo severe findings.\n\n" + marker.Summary(),
		"state":     "COMMENTED",
		"user":      map[string]any{"login": testBotLogin},
	}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model:       &sequenceModel{err: errors.New("provider rejected response schema")},
		reviewPages: [][]map[string]any{{summaryReview}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want error")
	}
	if fixture.state.lastSubmitReview != nil {
		t.Fatal("SubmitReview was called although a summary review already exists")
	}
	updated := fixture.state.lastUpdateReview
	if updated == nil {
		t.Fatal("the existing summary review was not updated")
	}
	body, ok := updated["body"].(string)
	if !ok || !strings.Contains(body, "Review failed during model analysis.") {
		t.Fatalf("updated body = %v, want the failure wording", updated["body"])
	}
}

// approvedSummaryReview is a prior bot review that approved an earlier head and
// owns the visible summary body.
func approvedSummaryReview(state string) map[string]any {
	return map[string]any{
		"id":        float64(42),
		"commit_id": testStaleHeadSHA,
		"body":      "## Review\n\nNo severe findings.\n\n" + marker.Summary(),
		"state":     state,
		"user":      map[string]any{"login": testBotLogin},
	}
}

// GitHub keeps a review's state when only its body is edited, so a failure that
// rewrites an approval would otherwise leave the approval standing.
func TestServiceDismissesAnApprovalItCanNoLongerStandBehind(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model:       &sequenceModel{err: errors.New("provider refused the prompt")},
		reviewPages: [][]map[string]any{{approvedSummaryReview("APPROVED")}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want error")
	}
	if fixture.state.lastDismissReview == nil {
		t.Fatal("the stale approval was not dismissed")
	}
	if fixture.state.dismissReviewPath != "/repos/owner/repo/pulls/7/reviews/42/dismissals" {
		t.Fatalf("dismiss path = %q, want the review dismissals path", fixture.state.dismissReviewPath)
	}
	if fixture.state.lastDismissReview["event"] != "DISMISS" {
		t.Fatalf("event = %v, want DISMISS", fixture.state.lastDismissReview["event"])
	}
	message, ok := fixture.state.lastDismissReview["message"].(string)
	if !ok || !strings.Contains(message, "earlier head") {
		t.Fatalf("message = %v, want an explanation naming the earlier head", fixture.state.lastDismissReview["message"])
	}
}

// Writing the failure body is best effort, but a standing approval satisfies
// branch protection, so the dismissal must not depend on that write.
func TestServiceDismissesTheApprovalEvenWhenTheNoticeCannotBeWritten(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model:              &sequenceModel{err: errors.New("provider refused the prompt")},
		reviewPages:        [][]map[string]any{{approvedSummaryReview("APPROVED")}},
		updateReviewStatus: http.StatusInternalServerError,
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want error")
	}
	if fixture.state.lastDismissReview == nil {
		t.Fatal("the stale approval survived a failed notice write")
	}
}

func TestServiceLeavesANonApprovalAlone(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model:       &sequenceModel{err: errors.New("provider refused the prompt")},
		reviewPages: [][]map[string]any{{approvedSummaryReview("COMMENTED")}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want error")
	}
	if fixture.state.lastDismissReview != nil {
		t.Fatalf("a review that never approved was dismissed: %v", fixture.state.lastDismissReview)
	}
}

func TestServiceReturnsTheReviewCauseWhenTheDismissalFails(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model:               &sequenceModel{err: errors.New("provider refused the prompt")},
		reviewPages:         [][]map[string]any{{approvedSummaryReview("APPROVED")}},
		dismissReviewStatus: http.StatusInternalServerError,
	})

	err := fixture.run(context.Background(), fixture.job())
	if err == nil {
		t.Fatal("Run: want error")
	}
	if !strings.Contains(err.Error(), "provider refused the prompt") {
		t.Fatalf("err = %q, want the review cause", err)
	}
	if strings.Contains(err.Error(), "dismiss review failed") {
		t.Fatalf("err = %q, want the dismissal failure kept out of the returned cause", err)
	}
	if fixture.state.lastUpdateReview == nil {
		t.Fatal("the failure notice was not written when the dismissal failed")
	}
}

// A successful review must never dismiss its own approval.
func TestServiceDismissesNothingOnASuccessfulReview(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reviewPages: [][]map[string]any{{approvedSummaryReview("APPROVED")}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fixture.state.lastDismissReview != nil {
		t.Fatalf("a successful review dismissed its own approval: %v", fixture.state.lastDismissReview)
	}
}

// A failed review must report the same detail table a successful one does, so a
// reader can tell which model answered and how far it got.
func TestServiceFailureReportsProgressInTheCommentAndTheCheck(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &sequenceModel{err: errors.New("provider refused the prompt")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want error")
	}

	notice := fixture.state.lastSubmitReview
	if notice == nil {
		t.Fatal("failure notice was not published")
	}
	body, ok := notice["body"].(string)
	if !ok {
		t.Fatalf("notice body = %v, want string", notice["body"])
	}
	for _, want := range []string{
		"<summary>Review details</summary>",
		// No model answered, because the first chunk failed. The table says so
		// rather than naming the configured model that never replied.
		"| Model | unknown |",
		"| Head | `a3c4f1c` |",
		"| Files reviewed | `1` |",
		"| Diff chunks | `1` |",
		"| Reached | the diff |",
		"provider refused the prompt",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("failure notice missing %q:\n%s", want, body)
		}
	}

	output, ok := fixture.state.lastUpdateCheckRun["output"].(map[string]any)
	if !ok {
		t.Fatalf("check output = %v, want object", fixture.state.lastUpdateCheckRun["output"])
	}
	checkSummary, ok := output["summary"].(string)
	if !ok {
		t.Fatalf("check summary = %v, want string", output["summary"])
	}
	if !strings.Contains(checkSummary, "provider refused the prompt") {
		t.Fatalf("check summary lost the cause:\n%s", checkSummary)
	}
	if !strings.Contains(checkSummary, "| Reached | the diff |") {
		t.Fatalf("check summary lost the progress:\n%s", checkSummary)
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
			name:    "model analysis",
			title:   "Review failed during model analysis.",
			options: serviceFixtureOptions{model: &sequenceModel{err: errors.New("provider refused")}},
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

			notice := fixture.state.lastSubmitReview
			if notice == nil {
				t.Fatal("failure notice was not published")
			}
			if notice["event"] != string(domain.ReviewDecisionComment) {
				t.Fatalf("event = %v, want COMMENT", notice["event"])
			}
			body, ok := notice["body"].(string)
			if !ok || !strings.Contains(body, stage.title) {
				t.Fatalf("notice body = %v, want %q", notice["body"], stage.title)
			}
		})
	}
}

func TestServiceRejectPublishesAFailureNotice(t *testing.T) {
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

	notice := fixture.state.lastSubmitReview
	if notice == nil {
		t.Fatal("failure notice was not published")
	}
	body, ok := notice["body"].(string)
	if !ok || !strings.Contains(body, "review queue is full") {
		t.Fatalf("notice body = %v, want the rejection cause", notice["body"])
	}
}

// The notice needs the same GitHub reads that failed in these stages, so it
// cannot be written. The check run still reports the failure.
func TestServiceSkipsTheNoticeWhenItsOwnGitHubCallIsTheFailure(t *testing.T) {
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
			if fixture.state.lastSubmitReview != nil {
				t.Fatalf("a second visible comment was created: %v", fixture.state.lastSubmitReview)
			}
		})
	}
}

func TestServiceReturnsTheReviewCauseWhenTheNoticeCannotBeWritten(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model:              &sequenceModel{err: errors.New("provider refused the prompt")},
		submitReviewStatus: http.StatusInternalServerError,
	})

	err := fixture.run(context.Background(), fixture.job())
	if err == nil {
		t.Fatal("Run: want error")
	}
	if !strings.Contains(err.Error(), "provider refused the prompt") {
		t.Fatalf("err = %q, want the review cause", err)
	}
	if strings.Contains(err.Error(), "submit review failed") {
		t.Fatalf("err = %q, want the notice failure kept out of the returned cause", err)
	}
}

func TestServiceReviewsTheSameHeadAgainAfterAFailureNotice(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &failThenSucceedModel{err: errors.New("provider refused the prompt")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("first Run: want error")
	}
	notice := fixture.state.lastSubmitReview
	if notice == nil {
		t.Fatal("failure notice was not published")
	}
	noticeBody, ok := notice["body"].(string)
	if !ok {
		t.Fatalf("notice body = %v, want string", notice["body"])
	}

	// The published notice is the pull request state the retry reads back.
	fixture.state.reviewPages = [][]map[string]any{{{
		"id":        float64(42),
		"commit_id": testHeadSHA,
		"body":      noticeBody,
		"state":     "COMMENTED",
		"user":      map[string]any{"login": testBotLogin},
	}}}
	fixture.state.reviewPageIndex = 0
	fixture.state.lastSubmitReview = nil

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "success" {
		t.Fatalf("conclusion = %v, want success on the retry", fixture.state.lastUpdateCheckRun["conclusion"])
	}
	if fixture.state.lastUpdateReview == nil {
		t.Fatal("the retry did not rewrite the existing comment")
	}
	rewritten, ok := fixture.state.lastUpdateReview["body"].(string)
	if !ok || strings.Contains(rewritten, "Review failed") {
		t.Fatalf("rewritten body = %v, want the normal review summary", fixture.state.lastUpdateReview["body"])
	}
	if fixture.state.lastSubmitReview == nil {
		t.Fatal("the retry did not publish its review")
	}
	if fixture.state.lastSubmitReview["event"] == string(domain.ReviewDecisionComment) {
		t.Fatal("the retry published a second visible comment instead of its review")
	}
}

type failingCollector struct{}

func (failingCollector) Collect(
	context.Context,
	domain.PullRequestRef,
	githubapp.PullRequest,
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

func TestServiceSuppressesHistoricalFindingsAndPublishesHighestImportanceWithinCap(t *testing.T) {
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
		minimumImportance:         9,
		maximumUnresolvedComments: 1,
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
					Title:      "Lower importance defect",
					Body:       "This finding waits because capacity is limited.",
					Importance: 9,
				},
				{
					Path:       "main.go",
					StartLine:  3,
					EndLine:    3,
					Title:      "Highest importance defect",
					Body:       "This finding uses the available capacity.",
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
	comments, ok := fixture.state.lastSubmitReview["comments"].([]any)
	if !ok || len(comments) != 1 {
		t.Fatalf("comments = %v, want one", fixture.state.lastSubmitReview["comments"])
	}
	comment, ok := comments[0].(map[string]any)
	if !ok {
		t.Fatalf("comment = %T, want object", comments[0])
	}
	body, ok := comment["body"].(string)
	if !ok || !strings.Contains(body, "Highest importance defect") {
		t.Fatalf("comment body = %v, want highest importance finding", comment["body"])
	}
}

func TestServiceRequestsChangesWithoutPublishingWhenThreadCapIsFull(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance:         9,
		maximumUnresolvedComments: 1,
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
	comments, ok := fixture.state.lastSubmitReview["comments"].([]any)
	if !ok || len(comments) != 0 {
		t.Fatalf("comments = %v, want none", fixture.state.lastSubmitReview["comments"])
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
	dismissReviewStatus             int
	reconcileErr                    error
	reconcileThreads                []githubapp.ReviewThread
	collector                       review.Collector
	model                           review.Model
	minimumImportance               int
	maximumUnresolvedComments       int
}

type serviceFixture struct {
	service    *review.Service
	state      *serviceServerState
	reconciler *recordingReconciler
	model      review.Model
}

type serviceServerState struct {
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
	lastUpdateReview                map[string]any
	lastCreateCheckRun              map[string]any
	lastUpdateCheckRun              map[string]any
	checkDetailsURL                 string
	submitReviewStatus              int
	updateReviewStatus              int
	dismissReviewStatus             int
	lastDismissReview               map[string]any
	dismissReviewPath               string
}

type recordingReconciler struct {
	callCount int
	threads   []githubapp.ReviewThread
	err       error
}

func (reconciler *recordingReconciler) Reconcile(
	context.Context,
	domain.ReviewJob,
) ([]githubapp.ReviewThread, error) {
	reconciler.callCount++
	return reconciler.threads, reconciler.err
}

type stubCollector struct{}

func (stubCollector) Collect(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
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
		nextCheckRunID:                  77,
		headSHA:                         testHeadSHA,
		headAfterAnalysis:               options.headAfterAnalysis,
		pullRequestStatus:               options.pullRequestStatus,
		pullRequestStatusAfterFirstRead: options.pullRequestStatusAfterFirstRead,
		reviewListStatus:                options.reviewListStatus,
		submitReviewStatus:              options.submitReviewStatus,
		updateReviewStatus:              options.updateReviewStatus,
		dismissReviewStatus:             options.dismissReviewStatus,
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
	if state.dismissReviewStatus == 0 {
		state.dismissReviewStatus = http.StatusOK
	}
	if state.pullRequestStatus == 0 {
		state.pullRequestStatus = http.StatusOK
	}
	if state.reviewListStatus == 0 {
		state.reviewListStatus = http.StatusOK
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "/issues/comments") {
			http.Error(writer, "issue comments forbidden", http.StatusForbidden)
			return
		}
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
	maximumUnresolvedComments := options.maximumUnresolvedComments
	if maximumUnresolvedComments == 0 {
		maximumUnresolvedComments = 10
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

	service := review.NewService(
		client,
		collector,
		model,
		reconciler,
		queue.NewKeyedLocker(),
		testBotLogin,
		minimumImportance,
		maximumUnresolvedComments,
		10*time.Minute,
		testClock(8*time.Second),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
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

	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/") && strings.HasSuffix(request.URL.Path, "/reviews") {
		writeServiceReviewPage(writer, request, state)
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
		serviceWriteJSON(writer, http.StatusOK, map[string]any{
			"id":        float64(42),
			"commit_id": body["commit_id"],
			"state":     "COMMENTED",
			"body":      body["body"],
			"user":      map[string]any{"login": testBotLogin},
		})
		return
	}

	if request.Method == http.MethodPut && strings.HasSuffix(request.URL.Path, "/dismissals") {
		if state.dismissReviewStatus != http.StatusOK {
			serviceWriteJSON(writer, state.dismissReviewStatus, map[string]any{
				"message": "dismiss review failed",
			})
			return
		}
		body, err := serviceReadJSONBody(request)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		state.lastDismissReview = body
		state.dismissReviewPath = request.URL.Path
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

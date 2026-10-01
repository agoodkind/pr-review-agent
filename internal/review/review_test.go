package review_test

import (
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

const (
	testHeadSHA     = "a3c4f1cac7f595bc824704b9d2a1f1191630dc32"
	testReviewModel = "fixture-review-model"
)

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

// testPublishedFinding is one finding that reached the pull request inline.
func testPublishedFinding() domain.Finding {
	return domain.Finding{
		Path:       "main.go",
		StartLine:  1,
		EndLine:    1,
		Title:      "Blocker",
		Body:       "Must fix before merge.",
		Importance: 9,
	}
}

func TestRenderBodyLeadsWithTheVerdictThenTheDetails(t *testing.T) {
	tests := []struct {
		name      string
		decision  domain.ReviewDecision
		published []domain.Finding
		blocking  []string
		verdict   string
	}{
		{
			name:      "approve",
			decision:  domain.ReviewDecisionApprove,
			published: nil,
			blocking:  nil,
			verdict:   "This review found no severe defects.",
		},
		{
			name:      "request changes over a published finding",
			decision:  domain.ReviewDecisionRequestChanges,
			published: []domain.Finding{testPublishedFinding()},
			blocking:  []string{"[main.go:1](https://github.com/owner/repo/pull/7#discussion_r1)"},
			verdict:   "Resolve the open inline findings.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary := testSummary()
			summary.Decision = test.decision
			summary.Published = test.published
			summary.Blocking = test.blocking

			body := review.RenderBody(summary)
			previous := -1
			for _, section := range []string{
				"### Summary", "### Changes",
				"### Verdict", "<summary>Review details</summary>",
			} {
				index := strings.Index(body, section)
				if index <= previous {
					t.Fatalf("section %q is missing or out of order:\n%s", section, body)
				}
				previous = index
			}
			if !strings.Contains(body, test.verdict) {
				t.Fatalf("body does not state verdict %q:\n%s", test.verdict, body)
			}
			visible := strings.Split(body, "<details>")[0]
			for _, unwanted := range []string{"This review is waiting on:", "discussion_r", "### Omissions", "Coverage", "The review read"} {
				if strings.Contains(visible, unwanted) {
					t.Fatalf("visible review contains redundant detail %q:\n%s", unwanted, body)
				}
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

func containsTypographicDash(value string) bool {
	for _, character := range value {
		switch character {
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
			return true
		}
	}
	return false
}

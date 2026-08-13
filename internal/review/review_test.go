package review_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

const testHeadSHA = "a3c4f1cac7f595bc824704b9d2a1f1191630dc32"

func TestDecisionForAllThreeDecisions(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		decision := review.DecisionFor(true, nil)
		if decision != domain.ReviewDecisionApprove {
			t.Fatalf("decision = %q, want %q", decision, domain.ReviewDecisionApprove)
		}
	})

	t.Run("comment", func(t *testing.T) {
		findings := []domain.Finding{{
			Path:       "main.go",
			StartLine:  1,
			EndLine:    1,
			Title:      "Note",
			Body:       "Low severity issue.",
			Importance: 6,
		}}
		decision := review.DecisionFor(true, findings)
		if decision != domain.ReviewDecisionComment {
			t.Fatalf("decision = %q, want %q", decision, domain.ReviewDecisionComment)
		}
	})

	t.Run("request changes", func(t *testing.T) {
		findings := []domain.Finding{{
			Path:       "main.go",
			StartLine:  1,
			EndLine:    1,
			Title:      "Blocker",
			Body:       "Must fix before merge.",
			Importance: config.BlockingImportance,
		}}
		decision := review.DecisionFor(true, findings)
		if decision != domain.ReviewDecisionRequestChanges {
			t.Fatalf("decision = %q, want %q", decision, domain.ReviewDecisionRequestChanges)
		}
	})
}

func TestIncompleteCoverageNeverApproves(t *testing.T) {
	decision := review.DecisionFor(false, nil)
	if decision != domain.ReviewDecisionComment {
		t.Fatalf("decision = %q, want %q", decision, domain.ReviewDecisionComment)
	}

	analysis := review.Analysis{
		Summary:          "Coverage incomplete.",
		CoverageComplete: false,
		Decision:         review.DecisionFor(false, nil),
	}
	if analysis.Decision == domain.ReviewDecisionApprove {
		t.Fatalf("incomplete analysis decision = %q, want non-approve", analysis.Decision)
	}
}

func TestRenderBodyContainsSummaryUnanchoredAndMarker(t *testing.T) {
	head := domain.HeadSHA(testHeadSHA)
	analysis := review.Analysis{
		Summary: "Summary text.",
		Unanchored: []domain.Finding{{
			Path:       "other.go",
			StartLine:  3,
			EndLine:    3,
			Title:      "Missing anchor",
			Body:       "Path is not in the diff.",
			Importance: 4,
		}},
	}

	body := review.RenderBody(head, analysis)
	if !strings.Contains(body, "Summary text.") {
		t.Fatalf("body missing summary: %q", body)
	}
	if !strings.Contains(body, "## Unanchored findings") {
		t.Fatalf("body missing unanchored heading: %q", body)
	}
	if !strings.Contains(body, "Missing anchor") {
		t.Fatalf("body missing unanchored finding: %q", body)
	}
	if !strings.Contains(body, marker.Review(head)) {
		t.Fatalf("body missing review marker: %q", body)
	}
}

func TestRenderInlineUsesRightSideRangesAndFindingMarkers(t *testing.T) {
	head := domain.HeadSHA(testHeadSHA)
	findings := []domain.Finding{{
		Path:       "main.go",
		StartLine:  4,
		EndLine:    6,
		Title:      "Range issue",
		Body:       "Multiline anchor.",
		Importance: 5,
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
}

func TestRenderedProseHasNoTypographicDashes(t *testing.T) {
	head := domain.HeadSHA(testHeadSHA)
	analysis := review.Analysis{
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

	body := review.RenderBody(head, analysis)
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
	changed, err := diff.ChangedRightLines(patch)
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
				Summary:          "First chunk summary.",
				CoverageComplete: true,
				Findings: []domain.Finding{{
					Path:       "main.go",
					StartLine:  2,
					EndLine:    2,
					Title:      "Duplicate",
					Body:       "Same finding.",
					Importance: 4,
				}},
			},
			{
				Summary:          "Second chunk summary.",
				CoverageComplete: true,
				Findings: []domain.Finding{
					{
						Path:       "main.go",
						StartLine:  2,
						EndLine:    2,
						Title:      "Duplicate",
						Body:       "Same finding.",
						Importance: 4,
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

	analysis, err := review.Analyze(context.Background(), model, input)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if model.callCount != len(chunks) {
		t.Fatalf("model call count = %d, want %d", model.callCount, len(chunks))
	}
	if analysis.Summary != "First chunk summary.\n\nSecond chunk summary." {
		t.Fatalf("summary = %q, want joined chunk summaries", analysis.Summary)
	}
	if analysis.CoverageComplete {
		t.Fatal("coverage complete = true, want false from binary file")
	}
	if len(analysis.Anchored) != 1 {
		t.Fatalf("anchored count = %d, want 1", len(analysis.Anchored))
	}
	if len(analysis.Unanchored) != 2 {
		t.Fatalf("unanchored count = %d, want 2", len(analysis.Unanchored))
	}
	if analysis.Decision != domain.ReviewDecisionComment {
		t.Fatalf("decision = %q, want %q", analysis.Decision, domain.ReviewDecisionComment)
	}
}

func TestAnalyzePromptsInjectWritingPolicyAndUntrustedInput(t *testing.T) {
	patch := strings.Join([]string{
		"@@ -1,1 +1,2 @@",
		" package main",
		"+added",
	}, "\n")
	changed, err := diff.ChangedRightLines(patch)
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
			CoverageComplete:  true,
		}},
	}

	model := &sequenceModel{
		results: []domain.ReviewResult{{
			Summary:          "Chunk summary.",
			CoverageComplete: true,
		}},
	}

	_, err = review.Analyze(context.Background(), model, input)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(model.prompts) != 1 {
		t.Fatalf("prompt count = %d, want 1", len(model.prompts))
	}
	prompt := model.prompts[0]
	if !strings.Contains(prompt, config.WritingPolicy) {
		t.Fatalf("prompt missing writing policy: %q", prompt)
	}
	if !strings.Contains(prompt, review.UntrustedInputPolicy) {
		t.Fatalf("prompt missing untrusted input policy: %q", prompt)
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
	changed, err := diff.ChangedRightLines(patch)
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
			CoverageComplete:  true,
		}},
	}

	model := &sequenceModel{
		results: []domain.ReviewResult{{
			Summary:          "Missing required finding fields.",
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

	_, err = review.Analyze(context.Background(), model, input)
	if err == nil {
		t.Fatal("Analyze invalid model result: want error")
	}
}

type sequenceModel struct {
	results   []domain.ReviewResult
	prompts   []string
	callCount int
	err       error
}

func (model *sequenceModel) Review(_ context.Context, prompt string) (domain.ReviewResult, error) {
	model.prompts = append(model.prompts, prompt)
	if model.err != nil {
		return domain.ReviewResult{}, model.err
	}
	if model.callCount >= len(model.results) {
		return domain.ReviewResult{}, errors.New("unexpected model call")
	}
	result := model.results[model.callCount]
	model.callCount++
	return result, nil
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

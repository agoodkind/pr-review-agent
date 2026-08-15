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
)

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

func TestRenderBodyIsOneShortSummaryAndMarker(t *testing.T) {
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
			body := review.RenderBody(head, test.decision)
			want := "## Review\n\n" + test.message + "\n\n" + marker.Summary() + "\n" + marker.Review(head)
			if body != want {
				t.Fatalf("body = %q, want %q", body, want)
			}
		})
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

	body := review.RenderBody(head, domain.ReviewDecisionApprove)
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

type contextBlockingModel struct{}

func (contextBlockingModel) Review(ctx context.Context, _ string) (domain.ReviewResult, error) {
	<-ctx.Done()
	return domain.ReviewResult{}, ctx.Err()
}

type panicModel struct{}

func (panicModel) Review(context.Context, string) (domain.ReviewResult, error) {
	panic("model panic")
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
	wantBody := review.RenderBody(domain.HeadSHA(testHeadSHA), domain.ReviewDecisionApprove)
	if fixture.state.lastSubmitReview["body"] != wantBody {
		t.Fatalf("body = %v, want %q", fixture.state.lastSubmitReview["body"], wantBody)
	}
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
	if !ok || !strings.Contains(summary, "Minimum importance: `9`") {
		t.Fatalf("summary = %v, want configured minimum importance", output["summary"])
	}
	if !strings.Contains(summary, "Findings observed: `1`") {
		t.Fatalf("summary = %v, want observed finding count", output["summary"])
	}
	if !strings.Contains(summary, "Findings eligible: `0`") {
		t.Fatalf("summary = %v, want eligible finding count", output["summary"])
	}
	if !strings.Contains(summary, "Prior bot review IDs: none") {
		t.Fatalf("summary = %v, want loaded review identifiers", output["summary"])
	}
	if !strings.Contains(summary, "Bot thread IDs: none") {
		t.Fatalf("summary = %v, want loaded thread identifiers", output["summary"])
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
	if fixture.state.lastSubmitReview != nil {
		t.Fatal("SubmitReview was called after reconciliation failure")
	}
	if fixture.state.lastUpdateCheckRun["conclusion"] != "failure" {
		t.Fatalf("conclusion = %v, want failure", fixture.state.lastUpdateCheckRun["conclusion"])
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

func TestServiceSuppressesHistoricalFindingsAndPublishesHighestImportanceWithinCap(t *testing.T) {
	historical := domain.Finding{
		Path:       "main.go",
		StartLine:  2,
		EndLine:    2,
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
					EndLine:    2,
					Title:      "Historical defect",
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
					StartLine:  2,
					EndLine:    2,
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
	reviewPages               [][]map[string]any
	headAfterAnalysis         string
	pullRequestStatus         int
	submitReviewStatus        int
	reconcileErr              error
	reconcileThreads          []githubapp.ReviewThread
	model                     review.Model
	minimumImportance         int
	maximumUnresolvedComments int
}

type serviceFixture struct {
	service    *review.Service
	state      *serviceServerState
	reconciler *recordingReconciler
	model      review.Model
}

type serviceServerState struct {
	requestOrder       []string
	reviewPages        [][]map[string]any
	reviewPageIndex    int
	checkRuns          []map[string]any
	nextCheckRunID     int64
	headSHA            string
	headAfterAnalysis  string
	pullRequestReads   int32
	pullRequestStatus  int
	lastSubmitReview   map[string]any
	lastUpdateReview   map[string]any
	lastCreateCheckRun map[string]any
	lastUpdateCheckRun map[string]any
	checkDetailsURL    string
	submitReviewStatus int
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
		"@@ -1,1 +1,2 @@",
		" package main",
		"+added",
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
			CurrentContent:    "package main\nadded\n",
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

func (model *serialGateModel) Review(context.Context, string) (domain.ReviewResult, error) {
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

	return domain.ReviewResult{
		CoverageComplete: true,
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
		reviewPages:        [][]map[string]any{{}},
		checkRuns:          []map[string]any{},
		nextCheckRunID:     77,
		headSHA:            testHeadSHA,
		headAfterAnalysis:  options.headAfterAnalysis,
		pullRequestStatus:  options.pullRequestStatus,
		submitReviewStatus: options.submitReviewStatus,
	}
	if len(options.reviewPages) > 0 {
		state.reviewPages = options.reviewPages
	}
	if state.submitReviewStatus == 0 {
		state.submitReviewStatus = http.StatusOK
	}
	if state.pullRequestStatus == 0 {
		state.pullRequestStatus = http.StatusOK
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
		stubCollector{},
		model,
		reconciler,
		queue.NewKeyedLocker(),
		testBotLogin,
		minimumImportance,
		maximumUnresolvedComments,
		10*time.Minute,
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

	if request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/pulls/") && strings.Contains(request.URL.Path, "/reviews/") {
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
	if state.reviewPageIndex >= len(state.reviewPages) {
		serviceWriteJSON(writer, http.StatusOK, []map[string]any{})
		return
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

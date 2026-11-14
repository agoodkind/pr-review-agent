package review_test

import (
	"context"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/review"
)

type conciseReportModel struct {
	*sequenceModel
	prompt string
}

func (model *conciseReportModel) Review(
	ctx context.Context,
	prompt string,
) (review.Completion, error) {
	review.RecordModelUsage(
		ctx,
		review.ModelUsage{
			RequestedModel:              "review-alias",
			Model:                       "review-model",
			Requests:                    1,
			ReportedRequests:            1,
			InputTokens:                 100,
			CachedInputTokens:           20,
			OutputTokens:                10,
			ReasoningTokens:             4,
			TotalTokens:                 110,
			Priced:                      true,
			EstimatedInputCostUSD:       0.001,
			EstimatedCachedInputCostUSD: 0.0001,
			EstimatedOutputCostUSD:      0.002,
			EstimatedCostUSD:            0.0031,
		},
	)

	return model.sequenceModel.Review(ctx, prompt)
}

func (model *conciseReportModel) Report(
	ctx context.Context,
	prompt string,
) (review.ReportCompletion, error) {
	model.prompt = prompt

	review.RecordModelUsage(
		ctx,
		review.ModelUsage{
			RequestedModel:   "report-alias",
			Model:            "unpriced-report-model",
			Requests:         1,
			ReportedRequests: 1,
			InputTokens:      200,
			OutputTokens:     30,
			TotalTokens:      230,
		},
	)

	return review.ReportCompletion{
		Model: "unpriced-report-model",
		Report: review.Report{
			Summary: "The change rejects invalid input. Callers receive a clear error. A third sentence should not be published.",
			Walkthrough: []string{
				"The change rejects invalid input.",
				"Validation checks the boundary.",
				"Validation checks the boundary.",
				"Errors include the invalid value.",
				"Existing valid values retain their behavior.",
				"Tests exercise the rejected values.",
				"A fifth distinct item should not be published.",
			},
		},
	}, nil
}

func TestCompletedReviewKeepsProseConciseAndFoldsActualUsage(t *testing.T) {
	model := &conciseReportModel{sequenceModel: &sequenceModel{results: []domain.ReviewResult{{}}}}

	fixture := newServiceFixture(t, serviceFixtureOptions{model: model})
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body := failureSummaryComment(t, fixture)

	visible, details, found := strings.Cut(body, "<details>")
	if !found {
		t.Fatalf("review has no collapsed details: %s", body)
	}

	for _, unwanted := range []string{
		"third sentence",
		"fifth distinct",
		"### Omissions",
		"Coverage",
		"tokens",
		"review-alias",
		"waiting on",
	} {
		if strings.Contains(visible, unwanted) {
			t.Fatalf("visible review contains %q: %s", unwanted, visible)
		}
	}

	if strings.Count(visible, "The change rejects invalid input.") != 1 ||
		strings.Count(visible, "Validation checks the boundary.") != 1 ||
		strings.Count(visible, "\n- ") != 4 {
		t.Fatalf("summary or changes repeat or exceed their limit: %s", visible)
	}

	for _, want := range []string{
		"| `requests` | `2` |",
		"| `input_tokens` | `300` |",
		"| `cached_input_tokens` | `20` |",
		"| `output_tokens` | `40` |",
		"| `reasoning_tokens` | `4` |",
		"| `total_tokens` | `340` |",
		"| `estimated_input_cost_usd` | `0.00100000` |",
		"| `estimated_cached_input_cost_usd` | `0.00010000` |",
		"| `estimated_output_cost_usd` | `0.00200000` |",
		"| `estimated_cost_usd` | `0.00310000` |",
		"| `priced_requests` | `1` |",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("Actual review details omitted %q: %s", want, details)
		}
	}

	if fixture.state.lastSubmitReview["event"] != string(domain.ReviewDecisionApprove) {
		t.Fatalf("event = %v, want APPROVE", fixture.state.lastSubmitReview["event"])
	}

	for _, unwanted := range []string{
		"Verdict:",
		"Changed files:",
		"Current blocking locations:",
		"Unread changes:",
		"Current inline discussions:",
	} {
		if strings.Contains(model.prompt, unwanted) {
			t.Fatalf("report prompt includes unused review data %q: %s", unwanted, model.prompt)
		}
	}
}

func TestCompletedReviewRefreshUpdatesFoldedThreadsAndPreservesUsage(t *testing.T) {
	model := &conciseReportModel{sequenceModel: &sequenceModel{results: []domain.ReviewResult{{}}}}

	fixture := newServiceFixture(
		t,
		serviceFixtureOptions{
			model: model,
			reconcileThreads: []githubapp.ReviewThread{
				botThreadAt("first-thread", 2, false),
				botThreadAt("second-thread", 5, false),
			},
		},
	)
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	fixture.state.threadNodes = threadNodesFor([]githubapp.ReviewThread{
		botThreadAt("first-thread", 2, true),
		botThreadAt("second-thread", 5, false),
	})
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("refresh Run: %v", err)
	}

	body := failureSummaryComment(t, fixture)

	visible, details, found := strings.Cut(body, "<details>")
	if !found || !strings.Contains(visible, "Resolve the open inline findings.") {
		t.Fatalf("refreshed summary omits the verdict or fold: %s", body)
	}

	for _, expected := range []string{
		"| `total_tokens` | `340` |",
	} {
		if !strings.Contains(details, expected) {
			t.Fatalf("refreshed details omit %q: %s", expected, details)
		}
	}
}

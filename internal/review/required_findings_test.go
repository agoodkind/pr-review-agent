package review_test

import (
	"fmt"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
)

func TestServicePresentsRequiredMetadataBeforeTheReport(t *testing.T) {
	finding := domain.Finding{
		Surface: domain.FindingPullRequestDescription, RuleID: "complete_sentences",
		StartLine: 1, EndLine: 1, Title: "Complete the description",
		Body:     "The description omits a subject and verb. Write a complete sentence that explains the summary change beneath `### Summary`.",
		Evidence: "Summary changes", Importance: 10,
		CorrectionAction: domain.CorrectionReplace,
		Suggestion:       "The summary displays required findings before the report.",
	}
	model := &sequenceModel{results: []domain.ReviewResult{{Findings: []domain.Finding{finding}}}}
	fixture, _ := newContentCollectionFixture(t, model, contentCollectionOptions{
		description: finding.Evidence, onlyMetadata: true,
	})
	if err := fixture.run(t.Context(), fixture.job()); err != nil {
		t.Fatal(err)
	}
	if len(fixture.state.issueComments) != 1 || len(fixture.state.submittedReviews) != 1 {
		t.Fatal("The metadata rejection must publish one summary and one native review.")
	}
	summaryID := fixture.state.issueComments[0]["id"]
	nativeID := fixture.state.submittedReviews[0]["id"]
	const reportStart = "<!-- pr-review-agent:report-section:start -->"
	const reportEnd = "<!-- pr-review-agent:report-section:end -->"
	_, report, found := strings.Cut(failureSummaryComment(t, fixture), reportStart)
	if !found {
		t.Fatal("The summary must identify the start of its report.")
	}
	report, _, found = strings.Cut(report, reportEnd)
	if !found {
		t.Fatal("The summary must identify the end of its report.")
	}
	report = strings.TrimSpace(report)
	wantNative := fmt.Sprintf("The finding %s remains unresolved ([View required changes](https://github.com/owner/repo/pull/7#issuecomment-%.0f)).", finding.Title, summaryID)
	assertPublished := func(t *testing.T) {
		t.Helper()
		body := failureSummaryComment(t, fixture)
		previous := -1
		for _, content := range []string{"### Verdict", finding.Title, finding.Body, finding.Suggestion, "\n### Summary\n", "\n### Changes\n", "<summary>Review details</summary>"} {
			position := strings.Index(body, content)
			if position <= previous {
				t.Errorf("The summary must display %q in order above its review details:\n%s", content, body)
				break
			}
			previous = position
		}
		if position := strings.Index(body, finding.Body); position < 0 || position > strings.Index(body, "<details>") {
			t.Error("The summary must expand the complete finding and correction above its collapsed sections.")
		}
		if position := strings.Index(body, finding.Suggestion); position < 0 || position > strings.Index(body, "<details>") {
			t.Error("The summary must expand the replacement text above its collapsed sections.")
		}
		if !strings.Contains(body, report) {
			t.Error("The refresh must preserve the complete original report.")
		}
		native := fixture.state.submittedReviews[0]
		body, ok := native["body"].(string)
		visible, _, _ := strings.Cut(body, "<!--")
		if !ok || native["state"] != "CHANGES_REQUESTED" || strings.TrimSpace(visible) != wantNative {
			t.Errorf("The native rejection must retain one explanatory sentence linked to the summary: %v.", native)
		}
		if len(fixture.state.issueComments) != 1 || fixture.state.issueComments[0]["id"] != summaryID || len(fixture.state.submittedReviews) != 1 || native["id"] != nativeID {
			t.Error("The refresh must update the original summary and native review.")
		}
	}
	t.Run("published", assertPublished)
	for _, scenario := range []string{"refresh", "legacy section order", "missing verdict start", "missing verdict end"} {
		t.Run(scenario, func(t *testing.T) {
			body := failureSummaryComment(t, fixture)
			switch scenario {
			case "legacy section order":
				body = strings.ReplaceAll(body, reportStart, "")
				body = strings.ReplaceAll(body, reportEnd, "")
				verdict := strings.Index(body, "<!-- pr-review-agent:verdict-section:start -->")
				summary := strings.Index(body, "\n### Summary\n")
				details := strings.Index(body, "<details>")
				if verdict < 0 || summary < 0 || details < 0 {
					t.Fatal("The saved summary must contain the report, verdict, and review details.")
				}
				if verdict < summary {
					body = body[:verdict] + body[summary:details] + body[verdict:summary] + body[details:]
				}
			case "missing verdict start":
				body = strings.Replace(body, "<!-- pr-review-agent:verdict-section:start -->", "", 1)
			case "missing verdict end":
				body = strings.Replace(body, "<!-- pr-review-agent:verdict-section:end -->", "", 1)
			}
			fixture.state.issueComments[0]["body"] = body
			job := fixture.job()
			job.DeliveryID = scenario
			if err := fixture.run(t.Context(), job); err != nil {
				t.Fatal(err)
			}
			assertPublished(t)
			if len(model.prompts) != 1 {
				t.Fatal("The verdict refresh must reuse the completed metadata assessment.")
			}
		})
	}
}

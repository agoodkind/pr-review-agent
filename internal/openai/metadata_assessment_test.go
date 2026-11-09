package openai_test

import (
	"context"
	"net/http"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestMetadataReassessmentUsesStructuredProviderCompletion(t *testing.T) {
	for _, scenario := range []string{
		"resolved",
		"open",
		"uncertain",
		"duplicate",
		"invalid",
		"malformed",
		"missing evidence",
		"null evidence",
		"provider failure",
	} {
		t.Run(
			scenario,
			func(t *testing.T) {
				client, server, state := newTestClient(t)
				defer server.Close()
				state.completionContent = `{"assessments":[{"finding_id":"1","resolution":"` + scenario + `","reason":"Current text establishes the decision.","evidence":"Current field.","start_line":1,"end_line":1}]}`
				expectError := scenario != "resolved" && scenario != "open" && scenario != "uncertain"
				if scenario == "duplicate" {
					state.completionContent = `{"assessments":[{"finding_id":"1","resolution":"open","reason":"Still applies.","evidence":"Current field.","start_line":1,"end_line":1},{"finding_id":"1","resolution":"resolved","reason":"Fixed.","evidence":"Current field.","start_line":1,"end_line":1}]}`
				}

				if scenario == "malformed" {
					state.completionContent = `{"assessments":`
				}

				if scenario == "missing evidence" {
					state.completionContent = `{"assessments":[{"finding_id":"1","resolution":"resolved","reason":"The field is empty.","start_line":1,"end_line":1}]}`
				}

				if scenario == "null evidence" {
					state.completionContent = `{"assessments":[{"finding_id":"1","resolution":"resolved","reason":"The field is empty.","evidence":null,"start_line":1,"end_line":1}]}`
				}

				if scenario == "provider failure" {
					state.statusSequence = []int{http.StatusBadGateway}
				}

				ctx, usage := review.WithUsageRecorder(context.Background())
				assessments, err := client.ReassessMetadata(
					ctx,
					review.MetadataReassessmentRequest{
						Prompt:    "Current metadata assessment input.",
						FindingID: "1",
						Source: diff.MetadataSource{
							Surface:   domain.FindingPullRequestDescription,
							Text:      "Current field.",
							StartLine: 1,
							EndLine:   1,
						},
					},
				)
				if (err != nil) != expectError {
					t.Fatalf("The provider error was %v; expected failure is %t.", err, expectError)
				}

				if !expectError &&
					(len(
						assessments,
					) != 1 || assessments[0].Resolution != domain.Resolution(scenario)) {
					t.Fatal("The provider did not preserve the explicit metadata decision.")
				}

				if usage.Summary().Requests != 1 {
					t.Fatal("The usage summary must include the metadata assessment request.")
				}
			},
		)
	}
}

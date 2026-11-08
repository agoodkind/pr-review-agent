package openai_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestNativeInspectionContractRequiresAnExplicitArrayWithoutTextFormat(t *testing.T) {
	for _, classification := range []bool{false, true} {
		stage := "review"
		if classification {
			stage = "classification"
		}
		for _, example := range []struct {
			name     string
			field    json.RawMessage
			accepted bool
		}{
			{name: "omitted"},
			{name: "null", field: json.RawMessage(`null`)},
			{name: "object", field: json.RawMessage(`{}`)},
			{name: "explicit_empty_array", field: json.RawMessage(`[]`), accepted: true},
		} {
			t.Run(stage+"/"+example.name, func(t *testing.T) {
				result := map[string]any{"overview": "The supplied source requires ordinary review.", "omissions_acceptable": true, "decision_reason": "", "findings": []any{}, "file_assessments": []any{}}
				if len(example.field) != 0 {
					result["inspection_requests"] = example.field
				}
				content, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				state := &testServerState{}
				state.streamResponse = func(writer http.ResponseWriter) {
					writeStreamFrames(writer, []map[string]any{
						responseTextDelta(string(content)),
						responseCompleted(string(content), "inspection-contract-model", map[string]any{"input_tokens": 11, "output_tokens": 5, "total_tokens": 16}),
					})
				}
				server := newProviderServer(state)
				defer server.Close()
				client := openai.NewClient(config.Config{ReviewPolicy: policytest.Load(t), MinimumImportance: 5, Providers: []config.ProviderConfig{{ID: "contract", API: config.ResponsesAPI, Model: "inspection-contract-model", BaseURL: mustParseURL(t, server.URL), APIKey: testAPIKeyValue(), OmitTextFormat: true}}}, server.Client())
				ctx, recorder := review.WithUsageRecorder(t.Context())
				var completion review.Completion
				if classification {
					completion, err = client.ClassifyFileContent(ctx, "Assess whether the supplied source requires content review.")
				} else {
					completion, err = client.Review(ctx, "Review the supplied source.")
				}
				if example.accepted {
					if err != nil || completion.Model != "inspection-contract-model" || len(completion.InspectionRequests) != 0 {
						t.Fatalf("The native client rejected an explicit empty request array: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "inspection") {
					t.Fatalf("The native client did not reject the invalid required inspection field: %v", err)
				}
				if _, sent := state.lastRequestBody["text"]; sent {
					t.Fatal("The fixture did not exercise the configured Clyde text-format omission.")
				}
				usage := recorder.Summary()
				if atomic.LoadInt32(&state.requestCount) != 1 || usage.Requests != 1 || usage.ReportedRequests != 1 || usage.InputTokens != 11 || usage.OutputTokens != 5 || usage.TotalTokens != 16 {
					t.Fatalf("The required-field validation lost native request usage: %+v", usage)
				}
			})
		}
	}
}

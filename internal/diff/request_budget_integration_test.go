package diff_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestRealGitPartitionIncludesCompleteProviderRequests(t *testing.T) {
	policy := policytest.Load(t)
	client := requestBudgetClient(policy, 80000)
	overhead, err := client.ReviewRequestBytes("")
	if err != nil {
		t.Fatal(err)
	}
	maximum := overhead + 16000
	client = requestBudgetClient(policy, maximum)
	change := largeGitFixture("replacement", 450)
	input := largeGitReviewInput(t, change)
	chunks, err := diff.ChunkInput(input, maximum)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := review.BoundModelReviewChunks(client, chunks, input.Files, 7, "", policy, maximum)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) < 2 {
		t.Fatal("Complete request accounting must divide the large Git change.")
	}
	for _, chunk := range bounded {
		prompt, err := review.BuildPrompt(chunk, 7, "", policy, maximum)
		if err != nil {
			t.Fatal(err)
		}
		payloads, err := client.ReviewRequestPayloads(prompt)
		if err != nil {
			t.Fatal(err)
		}
		for _, payload := range payloads {
			if len(payload.Body) > maximum {
				t.Fatalf("Provider %s requires %d bytes; the complete request budget is %d.", payload.ProviderID, len(payload.Body), maximum)
			}
			assertProviderRequestContent(t, policy, prompt, payload)
		}
	}
	assertLargeGitCoverage(t, change, input, bounded)
}

func TestGitReviewKeepsRequiredSourceWhenOptionalContextExceedsTheRequest(t *testing.T) {
	policy := policytest.Load(t)
	client := requestBudgetClient(policy, 80000)
	overhead, err := client.ReviewRequestBytes("")
	if err != nil {
		t.Fatal(err)
	}
	maximum := overhead + 5000
	change := largeGitFixture("replacement", 30)
	input := largeGitReviewInput(t, change)
	chunks, err := diff.ChunkInput(input, maximum)
	if err != nil {
		t.Fatal(err)
	}
	const required = "Required omission evidence: current-file-identity.\n"
	optional := strings.Repeat("Optional pull request and discussion context café 雪.\n", 5000)
	bounded, err := review.BoundModelReviewChunksWithContext(client, chunks, input.Files, 7, required, optional, policy, maximum)
	if err != nil {
		t.Fatal(err)
	}
	var data reviewrules.PromptData
	notice, err := policy.Render("review.context_omitted", data)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range bounded {
		prompt, err := review.BuildModelReviewPrompt(client, chunk, 7, required, optional, policy, maximum)
		if err != nil {
			t.Fatal(err)
		}
		size, err := client.ReviewRequestBytes(prompt)
		if err != nil {
			t.Fatal(err)
		}
		if size > maximum || !strings.Contains(prompt, required) || !strings.Contains(prompt, notice) || !utf8.ValidString(prompt) {
			t.Fatal("The full request must retain required evidence, disclose omitted optional context, and fit its byte budget.")
		}
	}
	assertLargeGitCoverage(t, change, input, bounded)
}

func TestClientRejectsOversizedCompleteRequestBeforeProviderCalls(t *testing.T) {
	policy := policytest.Load(t)
	prompt := "Review the supplied change."
	client := requestBudgetClient(policy, 80000)
	size, err := client.ReviewRequestBytes(prompt)
	if err != nil {
		t.Fatal(err)
	}
	client = requestBudgetClient(policy, size-1)
	completion, err := client.Review(t.Context(), prompt)
	var refusal *diff.PromptSizeError
	if !errors.As(err, &refusal) || refusal.Actual != size || refusal.Limit != size-1 || completion.Model != "" {
		t.Fatalf("The complete request must fail locally at its configured byte limit: result %+v, error %v.", completion, err)
	}
}

func TestCompleteProviderBudgetPreservesIndivisibleGitGap(t *testing.T) {
	policy := policytest.Load(t)
	client := requestBudgetClient(policy, 80000)
	overhead, err := client.ReviewRequestBytes("")
	if err != nil {
		t.Fatal(err)
	}
	maximum := overhead + 16000
	line := strings.Repeat("q", maximum*2)
	change := largeGitChange{before: "", after: line + "\n", status: "added", added: map[int]string{1: line}, removed: nil}
	input := largeGitReviewInput(t, change)
	chunks, err := diff.ChunkInput(input, maximum)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := review.BoundModelReviewChunks(client, chunks, input.Files, 7, "", policy, maximum)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) != 1 || bounded[0].CoverageComplete || len(bounded[0].Pieces) != 1 || !bounded[0].Pieces[0].Oversized {
		t.Fatal("Complete provider accounting must preserve the indivisible source gap.")
	}
	rows, err := diff.ChangedSourceRows(bounded[0])
	if err != nil || len(rows) != 0 {
		t.Fatal("The indivisible source line must not become completed review coverage.")
	}
}

func TestFullRequestContextBudgetPreservesRequiredUnicodeEvidence(t *testing.T) {
	policy := policytest.Load(t)
	client := requestBudgetClient(policy, 80000)
	required := "Required source evidence: café 雪.\n"
	size, err := client.ReviewRequestBytes(required)
	if err != nil {
		t.Fatal(err)
	}
	maximum := size + 3000
	optional := strings.Repeat("Supplementary café 雪 context.\n", 2000)
	prompt, err := review.BoundReviewPrompt(client, required, optional, maximum)
	if err != nil {
		t.Fatal(err)
	}
	actualSize, err := client.ReviewRequestBytes(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prompt, required) || !utf8.ValidString(prompt) || len(prompt) >= len(required)+len(optional) || actualSize > maximum {
		t.Fatal("The complete context budget must preserve required evidence and valid UTF-8 within the provider request limit.")
	}
}

func requestBudgetClient(policy reviewrules.Policy, maximum int) *openai.Client {
	providers := make([]config.ProviderConfig, 0, 3)
	for _, api := range []config.ProviderAPI{config.ResponsesAPI, config.ChatCompletionsAPI, config.GeminiAPI} {
		providers = append(providers, config.ProviderConfig{ID: string(api), API: api, Model: "request-size-measurement", ReasoningEffort: config.ReasoningLow})
	}
	return openai.NewClient(config.Config{Providers: providers, MinimumImportance: 7, ReviewPolicy: policy, ReviewMaxPromptBytes: maximum}, nil)
}

func assertProviderRequestContent(t *testing.T, policy reviewrules.Policy, prompt string, payload openai.ReviewRequestPayload) {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload.Body, &body); err != nil {
		t.Fatal(err)
	}
	var instructions string
	var schema json.RawMessage
	switch payload.API {
	case config.ResponsesAPI:
		if err := json.Unmarshal(body["instructions"], &instructions); err != nil {
			t.Fatal(err)
		}
		var native struct {
			Format struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"format"`
		}
		if err := json.Unmarshal(body["text"], &native); err != nil {
			t.Fatal(err)
		}
		schema = native.Format.Schema
	case config.ChatCompletionsAPI:
		var messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(body["messages"], &messages); err != nil || len(messages) != 2 || messages[0].Role != "system" {
			t.Fatal("The Chat request must include the configured system instructions.")
		}
		instructions = messages[0].Content
		var native struct {
			JSONSchema struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		}
		if err := json.Unmarshal(body["response_format"], &native); err != nil {
			t.Fatal(err)
		}
		schema = native.JSONSchema.Schema
	case config.GeminiAPI:
		var system struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		if err := json.Unmarshal(body["systemInstruction"], &system); err != nil || len(system.Parts) != 1 {
			t.Fatal("The Gemini request must include the configured system instructions.")
		}
		instructions = system.Parts[0].Text
		var native struct {
			Schema json.RawMessage `json:"responseJsonSchema"`
		}
		if err := json.Unmarshal(body["generationConfig"], &native); err != nil {
			t.Fatal(err)
		}
		schema = native.Schema
	}
	configuredSchema, err := openai.MarshalReviewSchema(policy)
	if err != nil {
		t.Fatal(err)
	}
	configuredPolicy, err := review.PolicyHeader(policy, 7, nil)
	if err != nil {
		t.Fatal(err)
	}
	var data reviewrules.PromptData
	data.Input, data.SchemaName, data.Schema = configuredPolicy, "review_result", string(configuredSchema)
	expectedInstructions, err := policy.Render("structured.output", data)
	if err != nil {
		t.Fatal(err)
	}
	var actualSchema, expectedSchema any
	if err := json.Unmarshal(schema, &actualSchema); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(configuredSchema, &expectedSchema); err != nil {
		t.Fatal(err)
	}
	if instructions != expectedInstructions || !reflect.DeepEqual(actualSchema, expectedSchema) || !strings.Contains(string(payload.Body), "changed.go") || len(payload.Body) <= len(prompt)+len(instructions)+len(schema) {
		t.Fatal("Request accounting omitted configured instructions, source, native schema, or JSON overhead.")
	}
}

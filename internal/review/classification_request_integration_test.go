package review_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestClassificationRequestUsesDedicatedTaskAndExactNativeBudget(t *testing.T) {
	policy := policytest.Load(t)
	providers := []config.ProviderConfig{
		{ID: "responses", API: config.ResponsesAPI, Model: "classification-model"},
		{ID: "chat", API: config.ChatCompletionsAPI, Model: "classification-model"},
		{ID: "gemini", API: config.GeminiAPI, Model: "classification-model"},
	}
	client := openai.NewClient(config.Config{ReviewPolicy: policy, MinimumImportance: 7, Providers: providers}, nil)
	overhead, err := client.ClassificationRequestBytes("")
	if err != nil {
		t.Fatal(err)
	}
	limit := overhead + 5000
	file := classificationGitFile(t, "credentials.vault", "$ANSIBLE_VAULT;1.1;AES256\n61616161\n", "$ANSIBLE_VAULT;1.1;AES256\n"+strings.Repeat("62626262\n", 100))
	pr := githubapp.PullRequest{Title: "Rotate a fixture token", Body: strings.Repeat("Optional parent context. ", limit)}
	prompt, err := review.BuildFileAssessmentPrompt(client, pr, []diff.FileContext{file}, file.Path, policy, limit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "File identity:") || !strings.Contains(prompt, "Evidence ID:") || !strings.Contains(prompt, file.Path) {
		t.Fatal("The bounded classification request omitted required current file evidence.")
	}
	payloads, err := client.ClassificationRequestPayloads(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if len(payloads) != len(providers) {
		t.Fatal("Classification omitted a configured provider.")
	}
	maximum := 0
	for index, payload := range payloads {
		if payload.ProviderID != providers[index].ID || len(payload.Body) > limit {
			t.Fatal("Classification changed provider order or exceeded the complete request budget.")
		}
		maximum = max(maximum, len(payload.Body))
		assertClassificationTask(t, payload, policy)
	}
	measured, err := client.ClassificationRequestBytes(prompt)
	if err != nil || measured != maximum {
		t.Fatalf("The classification measurement must equal the largest native payload: measured=%d maximum=%d error=%v", measured, maximum, err)
	}
	ordinaryBytes, err := client.ReviewRequestBytes(prompt)
	if err != nil || ordinaryBytes <= limit {
		t.Fatalf("The request must fit its classification task while exceeding the ordinary finding task budget: %d, %v", ordinaryBytes, err)
	}
	bounded := openai.NewClient(config.Config{ReviewPolicy: policy, Providers: providers, ReviewMaxPromptBytes: measured - 1}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = bounded.ClassifyFileContent(ctx, prompt)
	var executionSize *diff.PromptSizeError
	if !errors.As(err, &executionSize) || executionSize.Actual != measured || executionSize.Limit != measured-1 {
		t.Fatalf("Execution must enforce the same native classification payload before contacting providers: %v", err)
	}
	t.Logf("Complete request bytes: classification=%d ordinary=%d configured_limit=%d.", measured, ordinaryBytes, limit)
	_, err = review.BuildFileAssessmentPrompt(client, pr, []diff.FileContext{file}, file.Path, policy, overhead-1)
	var oversized *diff.PromptSizeError
	if !errors.As(err, &oversized) {
		t.Fatal("Classification must reject a budget smaller than its required native request.")
	}
}

func assertClassificationTask(t *testing.T, payload openai.ReviewRequestPayload, policy reviewrules.Policy) {
	t.Helper()
	instructions, schema := classificationPayloadParts(t, payload)
	header, err := review.ClassificationPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := policy.Catalog().Prompt(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instructions, header) || !strings.Contains(instructions, policy.FileReviewInstructions()) || strings.Contains(instructions, catalog) {
		t.Fatal("Classification must use its configured scope task without the finding catalog.")
	}
	writing, err := policy.Catalog().OutputPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instructions, writing) {
		t.Fatal("Classification must retain the configured policy for its generated explanations.")
	}
	classificationSchema, err := openai.MarshalClassificationSchema(policy)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := policy.Render("structured.output", reviewrules.PromptData{Input: header, SchemaName: "file_classification", Schema: string(classificationSchema)})
	if err != nil || instructions != expected {
		t.Fatalf("Classification must serialize the exact configured instructions and schema identifier: %v", err)
	}
	if payload.API == config.ResponsesAPI && string(classificationJSONField(t, payload.Body, "text", "format", "name")) != `"file_classification"` {
		t.Fatal("The Responses request changed the classification schema identifier.")
	}
	if payload.API == config.ChatCompletionsAPI && string(classificationJSONField(t, payload.Body, "response_format", "json_schema", "name")) != `"file_classification"` {
		t.Fatal("The chat request changed the classification schema identifier.")
	}
	ordinary, err := openai.MarshalReviewSchema(policy)
	if err != nil {
		t.Fatal(err)
	}
	assessment := classificationJSONField(t, schema, "properties", "file_assessments")
	if !bytes.Equal(classificationCanonicalJSON(t, assessment), classificationCanonicalJSON(t, classificationJSONField(t, ordinary, "properties", "file_assessments"))) {
		t.Fatal("Classification changed the shared assessment schema constraints.")
	}
	if string(classificationJSONField(t, schema, "properties", "findings", "maxItems")) != "0" {
		t.Fatal("The native classification schema must prohibit findings.")
	}
	ordinaryClient := openai.NewClient(config.Config{ReviewPolicy: policy, Providers: []config.ProviderConfig{{ID: payload.ProviderID, API: payload.API, Model: "classification-model"}}}, nil)
	ordinaryPayloads, err := ordinaryClient.ReviewRequestPayloads("Review supplied source.")
	if err != nil {
		t.Fatal(err)
	}
	ordinaryInstructions, _ := classificationPayloadParts(t, ordinaryPayloads[0])
	if !strings.Contains(ordinaryInstructions, catalog) {
		t.Fatal("Ordinary review must retain the complete finding catalog.")
	}
}

func classificationCanonicalJSON(t *testing.T, source json.RawMessage) json.RawMessage {
	t.Helper()
	source = bytes.TrimSpace(source)
	if len(source) > 0 && source[0] == '{' {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(source, &fields); err != nil {
			t.Fatal(err)
		}
		for name, value := range fields {
			fields[name] = classificationCanonicalJSON(t, value)
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	if len(source) > 0 && source[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(source, &items); err != nil {
			t.Fatal(err)
		}
		for index, value := range items {
			items[index] = classificationCanonicalJSON(t, value)
		}
		encoded, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	return source
}

func classificationPayloadParts(t *testing.T, payload openai.ReviewRequestPayload) (string, json.RawMessage) {
	t.Helper()
	var request struct {
		Instructions string `json:"instructions"`
		Messages     []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		SystemInstruction struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
	}
	if err := json.Unmarshal(payload.Body, &request); err != nil {
		t.Fatal(err)
	}
	switch payload.API {
	case config.ResponsesAPI:
		return request.Instructions, classificationJSONField(t, payload.Body, "text", "format", "schema")
	case config.ChatCompletionsAPI:
		if len(request.Messages) == 0 || request.Messages[0].Role != "system" {
			t.Fatal("The chat request omitted its system message.")
		}
		return request.Messages[0].Content, classificationJSONField(t, payload.Body, "response_format", "json_schema", "schema")
	case config.GeminiAPI:
		if len(request.SystemInstruction.Parts) != 1 {
			t.Fatal("The Gemini request omitted its system instruction.")
		}
		return request.SystemInstruction.Parts[0].Text, classificationJSONField(t, payload.Body, "generationConfig", "responseJsonSchema")
	default:
		t.Fatalf("The request uses an unsupported API: %s", payload.API)
		return "", nil
	}
}

func classificationJSONField(t *testing.T, source json.RawMessage, names ...string) json.RawMessage {
	t.Helper()
	for _, name := range names {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(source, &fields); err != nil {
			t.Fatal(err)
		}
		var found bool
		source, found = fields[name]
		if !found {
			t.Fatalf("The native request omitted %s.", name)
		}
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, source); err != nil {
		t.Fatal(err)
	}
	return compact.Bytes()
}

func TestClassificationDecoderRejectsFindingsBeforeRuleValidation(t *testing.T) {
	policy := policytest.Load(t)
	payload := []byte(`{"overview":"","omissions_acceptable":false,"decision_reason":"","file_assessments":[],"findings":[{"rule_id":"unknown-classification-rule"}]}`)
	_, err := domain.UnmarshalClassificationResult(payload)
	var contract *domain.FileAssessmentValidationError
	if !errors.As(err, &contract) || contract.Field != "findings" {
		t.Fatalf("Classification must reject nonempty findings before validating finding fields: %v", err)
	}
	_, err = domain.UnmarshalReviewResult(payload, policy.Catalog(), nil)
	if err == nil || errors.As(err, &contract) {
		t.Fatalf("Ordinary review must preserve its finding rule validation: %v", err)
	}
	valid := []byte(`{"overview":"","omissions_acceptable":false,"decision_reason":"","file_assessments":[],"findings":[]}`)
	if _, err := domain.UnmarshalClassificationResult(valid); err != nil {
		t.Fatal(err)
	}
}

func TestClassificationTaskRequiresRuntimeConfiguration(t *testing.T) {
	directory := filepath.Join("..", "..", "config")
	data, err := os.ReadFile(filepath.Join(directory, "review-prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var configuration map[string]json.RawMessage
	if err := json.Unmarshal(data, &configuration); err != nil {
		t.Fatal(err)
	}
	var templates map[string]string
	if err := json.Unmarshal(configuration["templates"], &templates); err != nil {
		t.Fatal(err)
	}
	delete(templates, "classification.system")
	configuration["templates"], err = json.Marshal(templates)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "review-prompts.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = reviewrules.LoadPolicy(filepath.Join(directory, "review-rules.json"), file)
	if err == nil || !strings.Contains(err.Error(), "classification.system") {
		t.Fatalf("Policy loading must identify the missing classification task: %v", err)
	}
}

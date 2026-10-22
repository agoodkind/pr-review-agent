package openai_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

type assessmentSchemaNode struct {
	Properties   map[string]assessmentSchemaNode `json:"properties"`
	Items        *assessmentSchemaNode           `json:"items"`
	Pattern      string                          `json:"pattern"`
	Description  string                          `json:"description"`
	MinimumItems int                             `json:"minItems"`
	MaximumItems int                             `json:"maxItems"`
}

func TestSDKReviewRequestsUseConfiguredAssessmentBounds(t *testing.T) {
	for _, limits := range []struct{ reasonBytes, evidenceReferences int }{{512, 8}, {64, 2}} {
		t.Run(fmt.Sprintf("reason_%d_evidence_%d", limits.reasonBytes, limits.evidenceReferences), func(t *testing.T) {
			policy := assessmentBoundPolicy(t, limits.reasonBytes, limits.evidenceReferences)
			var providers []config.ProviderConfig
			for _, api := range []config.ProviderAPI{config.ResponsesAPI, config.ChatCompletionsAPI, config.GeminiAPI} {
				providers = append(providers, config.ProviderConfig{ID: string(api), API: api, Model: "assessment-bounds", ReasoningEffort: config.ReasoningLow})
			}
			client := openai.NewClient(config.Config{Providers: providers, MinimumImportance: 7, ReviewPolicy: policy}, nil)
			payloads, err := client.ReviewRequestPayloads("Assess the supplied current file and its evidence references.")
			if err != nil {
				t.Fatal(err)
			}
			if len(payloads) != len(providers) {
				t.Fatal("The actual SDK serialization omitted a configured provider API.")
			}
			var data reviewrules.PromptData
			data.Total = limits.reasonBytes
			description, err := policy.Render("schema.file_assessment.reason", data)
			if err != nil {
				t.Fatal(err)
			}
			for _, payload := range payloads {
				node := assessmentRequestSchema(t, payload)
				assessment := node.Properties["file_assessments"].Items
				if assessment == nil {
					t.Fatal("The actual provider request omitted the file assessment schema.")
				}
				reason := assessment.Properties["reason"]
				pattern, err := regexp.Compile(reason.Pattern)
				if err != nil || reason.Pattern == "" {
					t.Fatalf("The reason constraint is not a usable pattern: %v", err)
				}
				if !pattern.MatchString(strings.Repeat("x", limits.reasonBytes)) || pattern.MatchString(strings.Repeat("x", limits.reasonBytes+1)) || pattern.MatchString("") {
					t.Fatal("The actual provider schema does not bound the generated reason using the configured limit.")
				}
				unicodeReason := strings.Repeat("界", limits.reasonBytes)
				if !pattern.MatchString(unicodeReason) || len(unicodeReason) <= limits.reasonBytes {
					t.Fatal("The schema character constraint must remain distinct from application UTF-8 byte validation.")
				}
				if reason.Description != description {
					t.Fatal("The actual SDK request omitted the configured byte-limit explanation.")
				}
				evidence := assessment.Properties["evidence"]
				if evidence.MinimumItems != 1 || evidence.MaximumItems != limits.evidenceReferences {
					t.Fatal("The actual provider schema does not apply the configured evidence-reference limit.")
				}
			}
		})
	}
}

func assessmentRequestSchema(t *testing.T, payload openai.ReviewRequestPayload) assessmentSchemaNode {
	t.Helper()
	var request struct {
		Text struct {
			Format struct {
				Schema assessmentSchemaNode `json:"schema"`
			} `json:"format"`
		} `json:"text"`
		ResponseFormat struct {
			JSONSchema struct {
				Schema assessmentSchemaNode `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
		GenerationConfig struct {
			Schema assessmentSchemaNode `json:"responseJsonSchema"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(payload.Body, &request); err != nil {
		t.Fatal(err)
	}
	switch payload.API {
	case config.ResponsesAPI:
		return request.Text.Format.Schema
	case config.ChatCompletionsAPI:
		return request.ResponseFormat.JSONSchema.Schema
	case config.GeminiAPI:
		return request.GenerationConfig.Schema
	default:
		t.Fatal("The request uses an unsupported provider API.")
		return assessmentSchemaNode{}
	}
}

func assessmentBoundPolicy(t *testing.T, reasonBytes, evidenceReferences int) reviewrules.Policy {
	t.Helper()
	configuration := filepath.Join("..", "..", "config")
	contents, err := os.ReadFile(filepath.Join(configuration, "review-prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prompts map[string]json.RawMessage
	if err := json.Unmarshal(contents, &prompts); err != nil {
		t.Fatal(err)
	}
	var classification reviewrules.FileClassificationPolicy
	if err := json.Unmarshal(prompts["file_classification"], &classification); err != nil {
		t.Fatal(err)
	}
	classification.MaximumReasonBytes = reasonBytes
	classification.MaximumEvidenceReferences = evidenceReferences
	prompts["file_classification"], err = json.Marshal(classification)
	if err != nil {
		t.Fatal(err)
	}
	contents, err = json.Marshal(prompts)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "review-prompts.json")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := reviewrules.LoadPolicy(filepath.Join(configuration, "review-rules.json"), path)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

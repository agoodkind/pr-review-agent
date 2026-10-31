package openai_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/policytest"
)

func TestRouterRequestsRequireCapabilitiesAcrossProviderAPIs(t *testing.T) {
	for _, api := range []config.ProviderAPI{config.ResponsesAPI, config.ChatCompletionsAPI} {
		for _, target := range []struct {
			name     string
			model    string
			costTier config.AutoRouterCostTier
			router   bool
		}{
			{name: "free", model: config.FreeRouterModel, router: true},
			{name: "auto", model: config.AutoRouterModel, router: true},
			{name: "auto_low", model: config.AutoRouterModel, costTier: config.AutoRouterCostLow, router: true},
			{name: "direct", model: testPrimaryModel},
		} {
			for _, stage := range []struct {
				name    string
				content string
				call    func(*testing.T, *openai.Client) error
			}{
				{name: "review", content: `{"findings":[]}`, call: func(t *testing.T, client *openai.Client) error {
					_, err := client.Review(t.Context(), "Review the supplied source.")
					return err
				}},
				{name: "classification", content: `{"findings":[]}`, call: func(t *testing.T, client *openai.Client) error {
					_, err := client.ClassifyFileContent(t.Context(), "Assess the supplied source.")
					return err
				}},
				{name: "report", content: `{"summary":"The change validates requests.","walkthrough":["The handler checks account identifiers."]}`, call: func(t *testing.T, client *openai.Client) error {
					_, err := client.Report(t.Context(), "Summarize the supplied change.")
					return err
				}},
				{name: "reconcile", content: `{"resolutions":[]}`, call: func(t *testing.T, client *openai.Client) error {
					_, err := client.Reconcile(t.Context(), "Assess the supplied discussion.")
					return err
				}},
				{name: "consolidate", content: `{"groups":[]}`, call: func(t *testing.T, client *openai.Client) error {
					_, err := client.Consolidate(t.Context(), "Group the supplied findings.")
					return err
				}},
			} {
				t.Run(string(api)+"/"+target.name+"/"+stage.name, func(t *testing.T) {
					state := &testServerState{completionContent: stage.content}
					if api == config.ChatCompletionsAPI {
						state.streamResponse = func(writer http.ResponseWriter) {
							writeStreamFrames(writer, []map[string]any{{
								"id": "routing-contract", "object": "chat.completion.chunk", "created": 1, "model": target.model,
								"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": stage.content}, "finish_reason": "stop"}},
							}})
						}
					}
					server := newProviderServer(state)
					defer server.Close()
					client := openai.NewClient(config.Config{
						ReviewPolicy: policytest.Load(t), MinimumImportance: 5,
						Providers: []config.ProviderConfig{{
							ID: "routing", API: api, Model: target.model,
							BaseURL: mustParseURL(t, server.URL), APIKey: testAPIKeyValue(), AutoRouterCostTier: target.costTier,
						}},
					}, server.Client())
					if err := stage.call(t, client); err != nil {
						t.Fatal(err)
					}
					routing, configured := state.lastRequestBody["provider"].(map[string]any)
					if configured != target.router || target.router && routing["require_parameters"] != true {
						t.Fatal("The SDK request must require supported parameters for each router independently of its cost tier.")
					}
					_, plugins := state.lastRequestBody["plugins"]
					if plugins != (target.costTier != "") {
						t.Fatal("Only an explicit cost tier may configure the auto-router plugin.")
					}
				})
			}
		}
	}
}

func TestDirectOpenRouterRequestsRequireSupportedParameters(t *testing.T) {
	for _, api := range []config.ProviderAPI{config.ResponsesAPI, config.ChatCompletionsAPI} {
		for _, endpoint := range []struct {
			url      string
			required bool
		}{
			{url: "https://openrouter.ai/api/v1", required: true},
			{url: "https://OPENROUTER.AI:443/api/v1", required: true},
			{url: "https://api.openai.com/v1"},
			{url: "https://openrouter.ai.example.com/v1"},
		} {
			t.Run(string(api)+"/"+endpoint.url, func(t *testing.T) {
				client := openai.NewClient(config.Config{
					ReviewPolicy: policytest.Load(t), MinimumImportance: 5,
					Providers: []config.ProviderConfig{{
						ID: "direct", API: api, Model: "nvidia/nemotron-3-ultra-550b-a55b:free",
						BaseURL: mustParseURL(t, endpoint.url),
					}},
				}, nil)
				payloads, err := client.ReviewRequestPayloads("Review the supplied source.")
				if err != nil || len(payloads) != 1 {
					t.Fatal("The direct-model request could not be serialized.")
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(payloads[0].Body, &body); err != nil {
					t.Fatal(err)
				}
				_, configured := body["provider"]
				if configured != endpoint.required {
					t.Fatal("The endpoint must determine OpenRouter capability enforcement for direct models.")
				}
				if configured && string(body["provider"]) != `{"require_parameters":true}` {
					t.Fatal("The direct-model request must require every supplied parameter.")
				}
			})
		}
	}
}

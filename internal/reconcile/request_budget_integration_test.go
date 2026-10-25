package reconcile_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/reconcile"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestReconciliationBatchesFitCompleteNativeProviderRequests(t *testing.T) {
	policy := policytest.Load(t)
	client := reconciliationBudgetClient(policy, config.MaximumPromptBytes)
	pullRequest := githubapp.PullRequest{Title: "Review Unicode source", Body: strings.Repeat("Required PR context café 雪.\n", 40)}
	threads := make([]reconcile.ThreadInput, 0, 12)
	for index := range 12 {
		threads = append(threads, reconcile.ThreadInput{
			ID:   fmt.Sprintf("thread-%d", index),
			Text: fmt.Sprintf("Thread %d required source:\n%s", index, strings.Repeat("café 雪 code anchor\n", 100)),
		})
	}
	initial, err := reconcile.BuildReconciliationRequests(threads[:1], pullRequest, policy, client, config.MaximumPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	single, err := client.ReconcileRequestBytes(initial[0].Prompt)
	if err != nil {
		t.Fatal(err)
	}
	maximum := single + 100
	batches, err := reconcile.BuildReconciliationRequests(threads, pullRequest, policy, client, maximum)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != len(threads) {
		t.Fatalf("Complete native accounting must split the raw-text batches into %d single-thread requests; got %d.", len(threads), len(batches))
	}
	for index, batch := range batches {
		payloads, err := client.ReconcileRequestPayloads(batch.Prompt)
		if err != nil {
			t.Fatal(err)
		}
		for _, payload := range payloads {
			if len(payload.Body) > maximum || !strings.Contains(batch.Prompt, threads[index].Text) || !strings.Contains(batch.Prompt, pullRequest.Body) || len(batch.ThreadIDs) != 1 || batch.ThreadIDs[0] != threads[index].ID {
				t.Fatal("Reconciliation must retain the required thread and PR context within each native request budget.")
			}
		}
	}
}

func TestReconciliationReportsAnIndivisibleThreadWithoutDroppingOtherBatches(t *testing.T) {
	policy := policytest.Load(t)
	client := reconciliationBudgetClient(policy, config.MaximumPromptBytes)
	pullRequest := githubapp.PullRequest{Title: "Review source anchors"}
	threads := []reconcile.ThreadInput{
		{ID: "oversized", Text: strings.Repeat("required anchor\n", 1000)},
		{ID: "reviewable", Text: "required small anchor"},
	}
	initial, err := reconcile.BuildReconciliationRequests(threads[1:], pullRequest, policy, client, config.MaximumPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	single, err := client.ReconcileRequestBytes(initial[0].Prompt)
	if err != nil {
		t.Fatal(err)
	}
	maximum := single + 100
	batches, err := reconcile.BuildReconciliationRequests(threads, pullRequest, policy, client, maximum)
	var refusal *diff.PromptSizeError
	if !errors.As(err, &refusal) || refusal.Limit != maximum || refusal.Actual <= maximum || len(batches) != 1 || batches[0].ThreadIDs[0] != "reviewable" {
		t.Fatalf("The oversized thread must report its complete byte requirement while the smaller thread remains reviewable: %v.", err)
	}
	client = reconciliationBudgetClient(policy, refusal.Actual-1)
	oversized, err := reconcile.BuildReconciliationRequests(threads[:1], pullRequest, policy, client, config.MaximumPromptBytes)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Reconcile(t.Context(), oversized[0].Prompt)
	if !errors.As(err, &refusal) {
		t.Fatal("The production client must reject the complete oversized reconciliation request before contacting a provider.")
	}
}

func reconciliationBudgetClient(policy reviewrules.Policy, maximum int) *openai.Client {
	providers := make([]config.ProviderConfig, 0, 3)
	for _, api := range []config.ProviderAPI{config.ResponsesAPI, config.ChatCompletionsAPI, config.GeminiAPI} {
		providers = append(providers, config.ProviderConfig{ID: string(api), API: api, Model: "request-size-measurement", ReasoningEffort: config.ReasoningLow})
	}
	return openai.NewClient(config.Config{Providers: providers, MinimumImportance: 7, ReviewPolicy: policy, ReviewMaxPromptBytes: maximum}, nil)
}

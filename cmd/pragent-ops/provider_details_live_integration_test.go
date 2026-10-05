//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestLiveProviderDetails(t *testing.T) {
	providerID := os.Getenv("PR_AGENT_LIVE_PROVIDER_ID")
	credentialPath := os.Getenv("PR_AGENT_LIVE_API_KEY_FILE")
	if providerID == "" && credentialPath == "" {
		t.Skip("Live provider credentials are not configured.")
	}
	if providerID == "" || credentialPath == "" {
		t.Fatal("Live provider testing requires a provider ID and credential file.")
	}
	runtimePath := filepath.Join("..", "..", "runtime.json")
	runtime, err := ReadProbeRuntime(runtimePath)
	if err != nil {
		t.Fatal("The runtime configuration could not be read.")
	}
	credential, err := cloudflareops.ReadCredential(credentialPath)
	if err != nil {
		t.Fatal("The credential file could not be read.")
	}
	signingKeyPath := os.Getenv("PR_AGENT_LIVE_SIGNING_KEY_FILE")
	cfg, err := probeConfig(runtime, providerID, string(credential), signingKeyPath)
	if err != nil {
		t.Fatal("The selected provider could not be configured.")
	}
	directory := os.Getenv("PR_AGENT_LIVE_DETAILS_OUTPUT_DIR")
	if directory == "" {
		directory = filepath.Join(t.TempDir(), "probe")
	}
	args := []string{"probe", "--provider", providerID, "--credential-file", credentialPath,
		"--runtime", runtimePath, "--output-dir", directory}
	if signingKeyPath != "" {
		args = append(args, "--signing-key-file", signingKeyPath)
	}
	var stdout, stderr bytes.Buffer
	probeErr := run(context.Background(), args, &stdout, &stderr)
	data, err := os.ReadFile(filepath.Join(directory, "result.json"))
	if err != nil {
		t.Fatal("The real provider probe did not produce diagnostics.")
	}
	var result struct {
		Success      bool                `json:"success"`
		Usage        review.UsageSummary `json:"usage"`
		HTTPStatuses map[int]int         `json:"http_statuses"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal("The real provider diagnostics could not be decoded.")
	}
	if len(result.Usage.ProviderAttempts) != 1 || result.Usage.Requests != 1 {
		t.Fatal("The probe did not record exactly one actual provider request.")
	}
	selected := cfg.Providers[0]
	attempt := result.Usage.ProviderAttempts[0]
	if attempt.Status.ProviderID != selected.ID || attempt.Status.Model != selected.Model ||
		attempt.API != config.ResolveProviderAPI(selected.API) || attempt.ReasoningEffort != config.ResolveReasoningEffort(selected.ReasoningEffort) ||
		attempt.OutputCapOmitted != selected.OmitMaxOutputTokens {
		t.Fatal("The recorded settings disagree with the actual selected provider.")
	}
	limit := selected.MaxOutputTokens
	if limit == 0 {
		limit = config.MaximumOutputTokens
	}
	if selected.OmitMaxOutputTokens {
		limit = 0
	}
	if attempt.MaxOutputTokens != limit || attempt.Completed != result.Success || (probeErr == nil) != result.Success {
		t.Fatal("The recorded output cap or completion status disagrees with the real request.")
	}
	if !result.Success && result.HTTPStatuses[attempt.HTTPStatus] != 1 {
		t.Fatal("The typed refusal status does not match the observed HTTP response.")
	}
	var summary review.Summary
	summary.Usage = result.Usage
	body := review.RenderBody(summary)
	if err := os.WriteFile(filepath.Join(directory, "review.md"), []byte(body), 0o600); err != nil {
		t.Fatal("The rendered review evidence could not be saved.")
	}
	visible, details, found := strings.Cut(body, "<details>")
	if !found || strings.Contains(visible, providerID) {
		t.Fatal("The provider diagnostics are not confined to the collapsed details.")
	}
	for _, value := range []string{providerID, selected.Model, string(attempt.API), string(attempt.ReasoningEffort)} {
		if !strings.Contains(details, value) {
			t.Fatal("The collapsed details omit an actual provider request setting.")
		}
	}
	for _, model := range result.Usage.Models {
		if model.ProviderID != selected.ID || model.RequestedModel != selected.Model {
			t.Fatal("The real token usage was assigned to the wrong provider.")
		}
		if model.Model != "" && !strings.Contains(details, model.Model) {
			t.Fatal("The collapsed details omit the model reported by the provider.")
		}
	}
	t.Logf("The real probe recorded %d request, %d usage reports, %d total tokens, and completion=%t.",
		result.Usage.Requests, result.Usage.ReportedRequests, result.Usage.TotalTokens, result.Success)
	t.Logf("The private diagnostics and rendered review were saved to %s.", directory)
}

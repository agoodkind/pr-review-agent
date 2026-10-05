//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestLiveProviderProbe(t *testing.T) {
	providerID := os.Getenv("PR_AGENT_LIVE_PROVIDER_ID")
	credentialPath := os.Getenv("PR_AGENT_LIVE_API_KEY_FILE")
	if providerID == "" && credentialPath == "" {
		t.Skip("Live provider credentials are not configured.")
	}
	if providerID == "" || credentialPath == "" {
		t.Fatal("Live provider testing requires PR_AGENT_LIVE_PROVIDER_ID and PR_AGENT_LIVE_API_KEY_FILE.")
	}
	signingKeyPath := os.Getenv("PR_AGENT_LIVE_SIGNING_KEY_FILE")
	runtimePath := filepath.Join("..", "..", "runtime.json")
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatal("The runtime configuration could not be read.")
	}
	var runtime probeRuntime
	if err = json.Unmarshal(data, &runtime); err != nil {
		t.Fatal("The runtime configuration is invalid.")
	}
	credential, err := cloudflareops.ReadCredential(credentialPath)
	if err != nil {
		t.Fatal("The live credential file could not be read.")
	}
	cfg, err := probeConfig(runtime, providerID, string(credential), signingKeyPath)
	if err != nil {
		t.Fatal("The selected runtime provider could not be configured.")
	}
	directory := filepath.Join(t.TempDir(), "probe")
	args := []string{
		"probe", "--provider", providerID, "--credential-file", credentialPath,
		"--runtime", runtimePath, "--output-dir", directory,
	}
	if signingKeyPath != "" {
		args = append(args, "--signing-key-file", signingKeyPath)
	}
	var stdout, stderr bytes.Buffer
	if err = run(context.Background(), args, &stdout, &stderr); err != nil {
		t.Fatal("The live provider probe did not complete successfully.")
	}
	data, err = os.ReadFile(filepath.Join(directory, "result.json"))
	if err != nil {
		t.Fatal("The probe result could not be read.")
	}
	var result struct {
		Provider        string              `json:"provider"`
		ConfiguredModel string              `json:"configured_model"`
		ReasoningEffort string              `json:"reasoning_effort"`
		Success         bool                `json:"success"`
		Usage           review.UsageSummary `json:"usage"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal("The probe result is invalid.")
	}
	selected := cfg.Providers[0]
	if !result.Success || result.Provider != selected.ID || result.ConfiguredModel != selected.Model || result.ReasoningEffort != string(selected.ReasoningEffort) {
		t.Fatal("The successful probe did not identify the selected runtime provider settings.")
	}
	usage := result.Usage
	if usage.Requests != 1 || usage.ReportedRequests != 1 || usage.InputTokens <= 0 || usage.OutputTokens <= 0 || usage.TotalTokens <= 0 {
		t.Fatal("The provider did not report token usage for exactly one review request.")
	}
	if len(usage.Models) != 1 || usage.Models[0].RequestedModel != selected.Model || usage.Models[0].Model == "" {
		t.Fatal("The usage report did not identify the requested and actual models.")
	}
}

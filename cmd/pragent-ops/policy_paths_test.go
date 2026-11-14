package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/opsruntime"
)

func TestReviewBodySummaryPrefixResolvesMixedPolicyPaths(t *testing.T) {
	configuration, err := filepath.Abs(filepath.Join("..", "..", "config"))
	if err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	rules, err := filepath.Rel(directory, filepath.Join(configuration, "review-rules.json"))
	if err != nil {
		t.Fatal(err)
	}

	prompts, err := filepath.Rel(directory, filepath.Join(configuration, "review-prompts.json"))
	if err != nil {
		t.Fatal(err)
	}

	runtime := opsruntime.ReviewBodyRuntime{
		RulesFile:          rules,
		PromptsFile:        prompts,
		ServiceFile:        filepath.Join(configuration, "review-service.json"),
		TemplatesDirectory: filepath.Join(configuration, "templates"),
	}
	data, err := json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}

	runtimePath := filepath.Join(directory, "runtime.json")
	if err := os.WriteFile(runtimePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var snapshot reviewBodySnapshot
	snapshot.Repository.Owner, snapshot.Repository.Name = "owner", "repo"
	snapshot.Number = 7
	snapshot.Comments = []githubapp.IssueComment{
		{ID: 2000, Author: "fixture-bot", Body: marker.EncodeState(marker.State{})},
	}
	prefix, err := reviewBodySummaryPrefix(
		reviewBodyOptions{runtime: runtimePath, linkSummary: true},
		snapshot,
		"fixture-bot",
	)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(
		prefix,
		"[Review summary](https://github.com/owner/repo/pull/7#issuecomment-2000)",
	) {
		t.Fatalf("The summary prefix did not render the configured permalink: %q", prefix)
	}
}

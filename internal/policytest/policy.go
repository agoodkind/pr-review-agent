// Package policytest loads the deployed review policy for existing test callers.
package policytest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// Load resolves policy paths from the repository runtime configuration.
func Load(tb testing.TB) reviewrules.Policy {
	tb.Helper()
	directory, err := os.Getwd()
	if err != nil {
		tb.Fatal(err)
	}
	for {
		runtimePath := filepath.Join(directory, "runtime.json")
		data, readErr := os.ReadFile(runtimePath)
		if readErr == nil {
			var paths struct {
				Rules   string `json:"REVIEW_RULES_FILE"`
				Prompts string `json:"REVIEW_PROMPTS_FILE"`
			}
			if err := json.Unmarshal(data, &paths); err != nil {
				tb.Fatal(err)
			}
			values := map[string]string{
				"REVIEW_RULES_FILE":   paths.Rules,
				"REVIEW_PROMPTS_FILE": paths.Prompts,
			}
			policy, err := config.LoadReviewPolicy(func(name string) (string, bool) {
				path, found := values[name]
				if !found || path == "" {
					return "", false
				}
				if !filepath.IsAbs(path) {
					path = filepath.Join(directory, path)
				}
				return path, true
			})
			if err != nil {
				tb.Fatal(err)
			}
			return policy
		}
		if !os.IsNotExist(readErr) {
			tb.Fatal(readErr)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			tb.Fatal("the repository runtime configuration was not found")
		}
		directory = parent
	}
}

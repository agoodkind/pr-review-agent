//go:build integration

package review_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestLivePublicInspectionPrimarySources(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_PUBLIC_INSPECTION") != "1" {
		t.Skip("Set PR_AGENT_LIVE_PUBLIC_INSPECTION=1 to read the public primary sources.")
	}
	fixture := newInspectionGitFixture(t)
	inspector, err := review.NewInspector(
		fixture.source,
		fixture.ref,
		fixture.files,
		policytest.Load(t),
		12000,
		6,
	)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp("", "pragent-public-inspection-live-")
	if err != nil {
		t.Fatal(err)
	}
	var receipts []struct {
		URL           string `json:"url"`
		ObservedAt    string `json:"observed_at"`
		OutputPath    string `json:"output_path"`
		OutputBytes   int    `json:"output_bytes"`
		OutputSHA256  string `json:"output_sha256"`
		ModelRequests int    `json:"model_requests"`
	}
	for _, source := range []struct {
		name     string
		url      string
		query    string
		expected string
	}{
		{
			name:     "tenstad-v0.2.1",
			url:      "https://raw.githubusercontent.com/tenstad/terraform-provider-remote/v0.2.1/internal/provider/resource_remote_file.go",
			query:    "ForceNew",
			expected: "ForceNew",
		},
		{
			name:     "opentofu-lifecycle",
			url:      "https://opentofu.org/docs/language/resources/behavior/",
			query:    "propagates",
			expected: "create_before_destroy",
		},
	} {
		output, err := inspector.Inspect(
			t.Context(),
			review.InspectionRequest{
				Operation: review.InspectionReadURL,
				Target:    source.url,
				Query:     source.query,
				Reason:    "Verify the original network migration against its dependency and primary documentation.",
			},
		)
		if err != nil || !strings.Contains(output, source.expected) || len(output) > 12000 {
			t.Fatalf(
				"The public primary source did not provide the requested bounded evidence: %s, %v",
				source.name,
				err,
			)
		}
		path := filepath.Join(directory, source.name+".txt")
		if err := os.WriteFile(path, []byte(output), 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(output))
		receipts = append(
			receipts,
			struct {
				URL           string `json:"url"`
				ObservedAt    string `json:"observed_at"`
				OutputPath    string `json:"output_path"`
				OutputBytes   int    `json:"output_bytes"`
				OutputSHA256  string `json:"output_sha256"`
				ModelRequests int    `json:"model_requests"`
			}{
				URL:           source.url,
				ObservedAt:    time.Now().UTC().Format(time.RFC3339),
				OutputPath:    path,
				OutputBytes:   len(output),
				OutputSHA256:  hex.EncodeToString(digest[:]),
				ModelRequests: 0,
			},
		)
	}
	data, err := json.MarshalIndent(receipts, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "receipt.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Public inspection receipts: %s", filepath.Join(directory, "receipt.json"))
}

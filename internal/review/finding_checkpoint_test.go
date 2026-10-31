package review_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/review"
)

func TestMetadataCheckpointRejectsInvalidFindingProvenance(t *testing.T) {
	summary := findingDetailsSummary()
	body := review.RenderBody(summary)
	if !review.MetadataMatches(body, summary.Head, summary.MetadataRevision) {
		t.Fatal("The legacy metadata checkpoint must remain readable.")
	}
	const prefix = "<!-- pr-review-agent:metadata:v1 "
	_, remaining, _ := strings.Cut(body, prefix)
	payload, _, _ := strings.Cut(remaining, " -->")
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &record); err != nil {
		t.Fatal(err)
	}
	for _, indices := range [][]int{{-1}, {1}, {0, 0}} {
		checkpoint, err := json.Marshal(struct {
			Prior []int `json:"prior"`
		}{Prior: indices})
		if err != nil {
			t.Fatal(err)
		}
		record["checkpoint"] = checkpoint
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		invalid := strings.Replace(body, payload, base64.RawURLEncoding.EncodeToString(encoded), 1)
		if review.MetadataMatches(invalid, summary.Head, summary.MetadataRevision) {
			t.Fatalf("The metadata checkpoint accepted invalid prior finding indices %v.", indices)
		}
	}
}

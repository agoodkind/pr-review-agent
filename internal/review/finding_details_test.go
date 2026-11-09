package review_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestFindingDetailsExcludePublishedInlineFindings(t *testing.T) {
	summary := findingDetailsSummary()
	summary.MetadataFindingsDisplay = loadFindingDisplay(t, reviewrules.FindingsExpanded)
	inline := domain.Finding{
		Path:       "src/review.go",
		StartLine:  2,
		EndLine:    2,
		Title:      "Check the response",
		Body:       "The response needs validation.",
		Importance: 10,
	}
	duplicate := inline
	duplicate.Path = "src/./review.go"
	duplicate.Title = " CHECK  the response "
	duplicate.EndLine = 9
	firstID, err := marker.FindingID(inline)
	if err != nil {
		t.Fatal(err)
	}

	duplicateID, err := marker.FindingID(duplicate)
	if err != nil || firstID != duplicateID {
		t.Fatal("The published finding and its restatement require the same identity.")
	}

	refused := domain.Finding{
		Path:       "src/other.go",
		StartLine:  3,
		EndLine:    3,
		Title:      "Check the unavailable input",
		Body:       "The unavailable input needs validation.",
		Importance: 10,
	}
	summary.Published = []domain.Finding{inline}
	summary.Metadata = append(summary.Metadata, duplicate)
	summary.Fallback = []domain.Finding{duplicate, refused}
	body := review.RenderBody(summary)
	if strings.Contains(body, inline.Body) || strings.Contains(body, duplicate.Title) {
		t.Fatal("The summary repeats an already published inline finding.")
	}

	if strings.Contains(body, summary.Metadata[0].Body) {
		t.Fatal("The summary must not repeat metadata findings from the rejection review.")
	}

	if strings.Count(body, refused.Body) != 1 {
		t.Fatal("The summary must display the refused file finding once.")
	}
}

func TestFindingDetailsUseLoadedDisplayWithoutChangingCheckpoints(t *testing.T) {
	summary := findingDetailsSummary()
	checkpoint := review.RenderVerdictBody(summary)
	decision := summary.Decision

	for _, display := range []reviewrules.FindingDisplay{
		reviewrules.FindingsExpanded,
		reviewrules.FindingsCollapsed,
		reviewrules.FindingsHidden,
	} {
		t.Run(
			string(display),
			func(t *testing.T) {
				summary.MetadataFindingsDisplay = loadFindingDisplay(t, display)
				body := review.RenderBody(summary)
				finding := summary.Metadata[0]
				if strings.Contains(body, finding.Body) || strings.Contains(body, finding.Title) {
					t.Fatal(
						"The summary must keep metadata findings in its hidden checkpoint for every display mode.",
					)
				}

				stored := retainedMetadataFindings(t, body)
				if len(stored) != 1 || stored[0].Body != finding.Body {
					t.Fatal("The summary must retain its recoverable metadata finding.")
				}

				if summary.Decision != decision || review.RenderVerdictBody(
					summary,
				) != checkpoint ||
					!strings.Contains(body, checkpoint) {
					t.Fatal("The display setting changed the decision or metadata checkpoint.")
				}
			},
		)
	}
}

func TestFailureMetadataFindingsRemainOnlyInTheCheckpoint(t *testing.T) {
	summary := findingDetailsSummary()
	summary.MetadataFindingsDisplay = loadFindingDisplay(t, reviewrules.FindingsExpanded)
	summary.ApprovalWithheld = true
	body := review.RenderFailureBody(
		summary,
		"The review could not finish.",
		"The provider request failed.",
	)
	if strings.Contains(body, summary.Metadata[0].Body) {
		t.Fatal("The failure summary must not repeat the metadata finding.")
	}

	stored := retainedMetadataFindings(t, body)
	if len(stored) != 1 || stored[0].Body != summary.Metadata[0].Body {
		t.Fatal("The failure summary must retain its metadata finding for recovery.")
	}
}

func TestRefusedFindingsRetainASeparateCheckpoint(t *testing.T) {
	summary := findingDetailsSummary()
	finding := domain.Finding{
		Path:       "src/refused.go",
		StartLine:  4,
		EndLine:    4,
		Title:      "Validate the result",
		Body:       "The result needs validation.",
		Evidence:   "private evidence",
		Claim:      "private claim",
		Importance: 10,
	}
	summary.Fallback = []domain.Finding{finding}
	summary.ApprovalWithheld = true
	body := review.RenderBody(summary)
	if _, found := marker.FindFinding(body); found {
		t.Fatal("A refused finding checkpoint must not claim an inline publication.")
	}

	const prefix = "<!-- pr-review-agent:fallback-record:v1 "
	_, checkpoint, found := strings.Cut(body, prefix)
	if !found {
		t.Fatal("The summary must retain the refused finding for later refreshes.")
	}

	payload, _, found := strings.Cut(checkpoint, " -->")
	if !found {
		t.Fatal("The refused finding checkpoint is incomplete.")
	}

	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}

	var record struct {
		Head     domain.HeadSHA   `json:"head"`
		Findings []domain.Finding `json:"findings"`
	}
	if err := json.Unmarshal(decoded, &record); err != nil {
		t.Fatal(err)
	}

	if record.Head != summary.Head || len(record.Findings) != 1 {
		t.Fatal("The refused finding checkpoint must identify its commit and finding.")
	}

	stored := record.Findings[0]
	originalID, err := marker.FindingID(finding)
	if err != nil {
		t.Fatal(err)
	}

	storedID, err := marker.FindingID(stored)
	if err != nil || storedID != originalID || stored.Body != finding.Body ||
		stored.Evidence != "" ||
		stored.Claim != "" {
		t.Fatal(
			"The checkpoint must retain the finding identity and correction without private evidence.",
		)
	}

	if summary.Fallback[0] != finding {
		t.Fatal("Rendering must not change the recorded finding.")
	}
}

func findingDetailsSummary() review.Summary {
	return review.Summary{
		Head:             domain.HeadSHA(testHeadSHA),
		Decision:         domain.ReviewDecisionRequestChanges,
		MetadataRevision: "current-metadata",
		CoverageComplete: true,
		Metadata: []domain.Finding{
			{
				Surface:    domain.FindingPullRequestTitle,
				StartLine:  1,
				EndLine:    1,
				Title:      "State the changed behavior",
				Body:       "The title needs a concrete changed behavior.",
				Importance: 10,
			},
		},
	}
}

func loadFindingDisplay(
	t *testing.T,
	display reviewrules.FindingDisplay,
) reviewrules.FindingDisplay {
	t.Helper()
	return loadFindingPolicy(t, display).MetadataFindingsDisplay()
}

func loadFindingPolicy(t *testing.T, display reviewrules.FindingDisplay) reviewrules.Policy {
	t.Helper()
	catalogPath := filepath.Join("..", "..", "config", "review-rules.json")
	promptsPath := filepath.Join("..", "..", "config", "review-prompts.json")
	data, err := os.ReadFile(promptsPath)
	if err != nil {
		t.Fatal(err)
	}

	var prompts map[string]json.RawMessage
	if err := json.Unmarshal(data, &prompts); err != nil {
		t.Fatal(err)
	}

	encodedDisplay, err := json.Marshal(display)
	if err != nil {
		t.Fatal(err)
	}

	prompts["metadata_findings_display"] = encodedDisplay
	data, err = json.Marshal(prompts)
	if err != nil {
		t.Fatal(err)
	}

	configuredPath := filepath.Join(t.TempDir(), "review-prompts.json")
	if err := os.WriteFile(configuredPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	policy, err := reviewrules.LoadPolicy(catalogPath, configuredPath)
	if err != nil {
		t.Fatal(err)
	}

	return policy
}

package domain

import (
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
)

func TestEndToEndCommentWithIncompleteCoverage(t *testing.T) {
	result := ReviewResult{
		Summary:          "Coverage incomplete.",
		CoverageComplete: false,
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	findings := []Finding{{
		Path:       "main.go",
		StartLine:  1,
		EndLine:    1,
		Title:      "Blocker",
		Body:       "Would request changes if coverage were complete.",
		Importance: config.BlockingImportance,
	}}
	for _, finding := range findings {
		if err := finding.Validate(); err != nil {
			t.Fatalf("finding validate: %v", err)
		}
	}
	if result.CoverageComplete {
		t.Fatal("coverage_complete = true, want false")
	}
}

func TestParseHeadSHAAcceptsSHA1AndSHA256(t *testing.T) {
	sha1 := "a3c4f1cac7f595bc824704b9d2a1f1191630dc32"
	sha256 := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	if _, err := ParseHeadSHA(sha1); err != nil {
		t.Fatalf("ParseHeadSHA(sha1): %v", err)
	}
	if _, err := ParseHeadSHA(sha256); err != nil {
		t.Fatalf("ParseHeadSHA(sha256): %v", err)
	}
}

func TestParseHeadSHARejectsUppercaseNonhexadecimalAndWrongLength(t *testing.T) {
	cases := []string{
		"A3C4F1CAC7F595BC824704B9D2A1F1191630DC32",
		"not-a-sha",
		"abc",
		"a3c4f1cac7f595bc824704b9d2a1f1191630dc3",
	}
	for _, value := range cases {
		if _, err := ParseHeadSHA(value); err == nil {
			t.Fatalf("ParseHeadSHA(%q) = nil, want error", value)
		}
	}
}

func TestParseReviewDecisionRejectsUnknownValue(t *testing.T) {
	if _, err := ParseReviewDecision("SHIP_IT"); err == nil {
		t.Fatal("ParseReviewDecision unknown value: want error")
	}
}

func TestParseResolutionRejectsUnknownValue(t *testing.T) {
	if _, err := ParseResolution("maybe"); err == nil {
		t.Fatal("ParseResolution unknown value: want error")
	}
}

func TestFindingValidateRejectsEmptyFieldsInvalidLinesAndImportance(t *testing.T) {
	valid := Finding{
		Path:       "internal/app/handler.go",
		StartLine:  10,
		EndLine:    12,
		Title:      "Missing check",
		Body:       "Validate input before use.",
		Importance: 7,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid finding: %v", err)
	}

	cases := []Finding{
		{Path: "", StartLine: 1, EndLine: 1, Title: "t", Body: "b", Importance: 1},
		{Path: "a.go", StartLine: 0, EndLine: 1, Title: "t", Body: "b", Importance: 1},
		{Path: "a.go", StartLine: 5, EndLine: 3, Title: "t", Body: "b", Importance: 1},
		{Path: "a.go", StartLine: 1, EndLine: 1, Title: "", Body: "b", Importance: 1},
		{Path: "a.go", StartLine: 1, EndLine: 1, Title: "t", Body: "", Importance: 1},
		{Path: "a.go", StartLine: 1, EndLine: 1, Title: "t", Body: "b", Importance: 0},
		{Path: "a.go", StartLine: 1, EndLine: 1, Title: "t", Body: "b", Importance: 11},
	}
	for index, finding := range cases {
		if err := finding.Validate(); err == nil {
			t.Fatalf("case %d: want validation error", index)
		}
	}
}

func TestReviewResultValidateRejectsDuplicateFindings(t *testing.T) {
	finding := Finding{
		Path:       "a.go",
		StartLine:  1,
		EndLine:    1,
		Title:      "Issue",
		Body:       "Details",
		Importance: 3,
	}
	result := ReviewResult{
		Summary:          "Summary",
		CoverageComplete: true,
		Findings:         []Finding{finding, finding},
	}
	if err := result.Validate(); err == nil {
		t.Fatal("duplicate findings: want error")
	}
}

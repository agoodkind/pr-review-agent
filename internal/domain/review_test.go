package domain

import "testing"

// A model answer carries findings and nothing else. It used to carry a coverage
// boolean the model was required to fill in blind, and the run believed it.
func TestReviewResultCarriesOnlyFindings(t *testing.T) {
	result := ReviewResult{Findings: []Finding{{
		Path:       "main.go",
		StartLine:  1,
		EndLine:    1,
		Title:      "Blocker",
		Body:       "The changed line breaks core behavior.",
		Evidence:   "return err",
		Suggestion: "",
		Importance: 7,
	}}}
	if err := result.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(result.Findings))
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
	for _, value := range []string{"MERGE", "SHIP_IT"} {
		if _, err := ParseReviewDecision(value); err == nil {
			t.Fatalf("ParseReviewDecision(%q): want error", value)
		}
	}
}

func TestParseReviewDecisionAcceptsCommentEvent(t *testing.T) {
	decision, err := ParseReviewDecision("COMMENT")
	if err != nil {
		t.Fatalf("ParseReviewDecision(COMMENT): %v", err)
	}
	if decision != ReviewDecisionComment {
		t.Fatalf("decision = %q, want %q", decision, ReviewDecisionComment)
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
		Suggestion: "",
		Importance: 7,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid finding: %v", err)
	}

	invalidSuggestion := valid
	invalidSuggestion.Suggestion = "```go\nunsafe()\n```"
	if err := invalidSuggestion.Validate(); err == nil {
		t.Fatal("fenced suggestion: want validation error")
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

func TestValidateThreadResolutionsRejectsDuplicatesAndInvalidValues(t *testing.T) {
	valid := ThreadResolution{
		ThreadNodeID: "thread-1",
		Resolution:   ResolutionResolved,
		Reason:       "fixed",
	}
	if err := ValidateThreadResolutions([]ThreadResolution{valid}); err != nil {
		t.Fatalf("ValidateThreadResolutions: %v", err)
	}

	duplicate := []ThreadResolution{valid, valid}
	if err := ValidateThreadResolutions(duplicate); err == nil {
		t.Fatal("duplicate thread ids: want error")
	}

	invalid := ThreadResolution{ThreadNodeID: "thread-2", Resolution: Resolution("nope"), Reason: "x"}
	if err := invalid.Validate(); err == nil {
		t.Fatal("invalid resolution: want error")
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
		Findings: []Finding{finding, finding},
	}
	if err := result.Validate(); err == nil {
		t.Fatal("duplicate findings: want error")
	}
}

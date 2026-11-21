package review_test

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestAcceptedFileScopeUsesConfiguredDetails(t *testing.T) {
	policy := policytest.Load(t)

	var data reviewrules.PromptData

	note, err := policy.Render("coverage.accepted", data)
	if err != nil {
		t.Fatal(err)
	}

	assessment := domain.FileAssessment{
		ContentReviewRequirement: domain.ContentReviewUnnecessary,
		Path:                     "secrets/vault.yml",
		Identity:                 "pinned-file",
		ContentKind:              "encrypted",
		Action:                   "exclude",
		Reason:                   "The rotation changes encrypted credentials and retains the existing secret references.",
		Evidence:                 []string{"file:pinned-file", "changed_files"},
	}

	for _, display := range []reviewrules.FindingDisplay{
		reviewrules.FindingsCollapsed,
		reviewrules.FindingsExpanded,
		reviewrules.FindingsHidden,
	} {
		t.Run(
			string(display),
			func(t *testing.T) {
				var summary review.Summary

				summary.Head = domain.HeadSHA(strings.Repeat("a", 40))
				summary.Decision = domain.ReviewDecisionApprove
				summary.OmissionsAccepted = true
				summary.FileAssessmentNote = note
				summary.FileAssessments = []domain.FileAssessment{assessment}
				summary.FileAssessmentsDisplay = display
				summary.FileAssessmentMaximumBytes = policy.FileAssessmentMaximumBytes()

				body := rendered(t)(review.RenderBody(summary))
				if strings.Contains(body, "### Omissions") ||
					strings.Contains(body, "The agent could not review") {
					t.Fatal("An accepted content exclusion appeared as an unfinished review.")
				}

				assertSavedFileAssessments(t, body, summary.FileAssessments)
				assessmentPosition := strings.Index(body, "#### File assessments")
				details := strings.Index(body, "<details>")
				if display == reviewrules.FindingsExpanded &&
					(assessmentPosition < 0 || assessmentPosition > details) {
					t.Fatal("The expanded assessment must appear before review details.")
				}

				if display != reviewrules.FindingsExpanded && assessmentPosition >= 0 {
					t.Fatal("Review details must omit file assessment diagnostics.")
				}

				summary.OmissionsAccepted = false
				summary.ApprovalWithheld = true
				summary.FileAssessments[0].Action = domain.FileActionGap

				pending := rendered(t)(review.RenderBody(summary))
				assertSavedFileAssessments(t, pending, summary.FileAssessments)
				if !strings.Contains(pending, "<!-- pr-review-agent:approval-withheld -->") {
					t.Fatal("An unresolved assessment must preserve withheld approval.")
				}
			},
		)
	}

	if policy.FileAssessmentsDisplay() != reviewrules.FindingsCollapsed {
		t.Fatal("The deployed policy does not collapse file assessment details.")
	}
}

func TestCheckpointPolicySurvivesStorageAndRejectsChangedContext(t *testing.T) {
	policy := policytest.Load(t)

	var state marker.State

	state.RunID = "checkpoint-policy-test"
	state.Status = marker.StateReviewing
	state = state.WithPolicyRevision(policy.Fingerprint())
	state.LastReviewed = domain.HeadSHA(strings.Repeat("b", 40))
	state.Completed = []string{"completed-fragment"}
	state.Pending = []string{"pending-fragment"}

	stored, valid := marker.DecodeState(marker.EncodeState(state))
	if !valid || stored.PolicyRevision != state.PolicyRevision {
		t.Fatal("The stored checkpoint did not retain its review policy identity.")
	}

	resumed := stored.WithPolicyRevision(policy.Fingerprint())
	if len(resumed.Completed) != 1 || len(resumed.Pending) != 1 {
		t.Fatal("An unchanged review context discarded completed work.")
	}

	changed := stored.WithPolicyRevision("changed-review-policy")
	if len(changed.Completed) != 0 || len(changed.Pending) != 0 ||
		changed.LastReviewed != stored.LastReviewed {
		t.Fatal("A changed importance policy reused an incompatible checkpoint.")
	}

	changed = stored.WithPolicyRevision("changed-comparison")
	if len(changed.Completed) != 0 || len(changed.Pending) != 0 {
		t.Fatal("A changed comparison reused an incompatible checkpoint.")
	}
}

func assertSavedFileAssessments(t *testing.T, body string, expected []domain.FileAssessment) {
	t.Helper()
	_, encoded, found := strings.Cut(body, "<!-- pr-review-agent:file-assessments:v1 ")
	if len(expected) == 0 {
		if found {
			t.Fatal("The summary invented file assessments.")
		}

		return
	}

	if !found {
		t.Fatal("The summary omitted its durable file assessments.")
	}

	encoded, _, _ = strings.Cut(encoded, " -->")
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}

	var restored []domain.FileAssessment
	if err := json.Unmarshal(payload, &restored); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(restored, expected) {
		t.Fatalf("The summary changed file assessments: got %+v, want %+v", restored, expected)
	}
}

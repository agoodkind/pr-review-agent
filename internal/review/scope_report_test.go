package review_test

import (
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
		Path: "secrets/vault.yml", Identity: "pinned-file", ContentKind: "encrypted", Action: "exclude",
		Reason:   "The rotation changes encrypted credentials and retains the existing secret references.",
		Evidence: []string{"file:pinned-file", "changed_files"},
	}
	for _, display := range []reviewrules.FindingDisplay{reviewrules.FindingsCollapsed, reviewrules.FindingsExpanded, reviewrules.FindingsHidden} {
		t.Run(string(display), func(t *testing.T) {
			var summary review.Summary
			summary.Head = domain.HeadSHA(strings.Repeat("a", 40))
			summary.Decision = domain.ReviewDecisionApprove
			summary.OmissionsAccepted = true
			summary.FileAssessmentNote = note
			summary.FileAssessments = []domain.FileAssessment{assessment}
			summary.FileAssessmentsDisplay = display
			summary.FileAssessmentMaximumBytes = policy.FileAssessmentMaximumBytes()
			body := review.RenderBody(summary)
			if strings.Contains(body, "### Omissions") || strings.Contains(body, "could not be reviewed") {
				t.Fatal("An accepted content exclusion appeared as an unfinished review.")
			}
			details := strings.Index(body, "<details>")
			if strings.Index(body, note) < details || !strings.Contains(body, "| All file content inspected | no |") || !strings.Contains(body, "| Review scope assessed | yes |") {
				t.Fatal("An accepted exclusion must report completed scope and its configured explanation inside review details.")
			}
			assessmentPosition := strings.Index(body, "#### File assessments")
			if display == reviewrules.FindingsCollapsed && (assessmentPosition < details || assessmentPosition < 0) {
				t.Fatal("The collapsed file assessment did not appear inside review details.")
			}
			if display == reviewrules.FindingsExpanded && (assessmentPosition < 0 || assessmentPosition > details) {
				t.Fatal("The expanded file assessment did not appear before review details.")
			}
			if display == reviewrules.FindingsHidden && assessmentPosition >= 0 {
				t.Fatal("The hidden file assessment appeared in the visible report.")
			}
			summary.OmissionsAccepted = false
			summary.ApprovalWithheld = true
			summary.FileAssessments[0].Action = domain.FileActionGap
			pending := review.RenderBody(summary)
			if strings.Contains(pending, note) || !strings.Contains(pending, "| Review scope assessed | no |") {
				t.Fatal("A real assessment gap must not claim an accepted exclusion or completed scope.")
			}
		})
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
	if len(changed.Completed) != 0 || len(changed.Pending) != 0 || changed.LastReviewed != stored.LastReviewed {
		t.Fatal("A changed importance policy reused an incompatible checkpoint.")
	}
	changed = stored.WithPolicyRevision("changed-comparison")
	if len(changed.Completed) != 0 || len(changed.Pending) != 0 {
		t.Fatal("A changed comparison reused an incompatible checkpoint.")
	}
}

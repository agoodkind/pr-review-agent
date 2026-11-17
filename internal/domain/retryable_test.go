package domain_test

import (
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
)

func TestOutcomeRetryEligibilityAndLegacyMarkers(t *testing.T) {
	for _, test := range []struct {
		name        string
		disposition domain.AssessmentDisposition
		classes     []string
		declined    bool
		want        bool
	}{
		{"panic", domain.AssessmentFailed, []string{"panic"}, false, false},
		{
			"mixed",
			domain.AssessmentIncomplete,
			[]string{"panic", "provider_unavailable"},
			false,
			true,
		},
		{"empty", domain.AssessmentFailed, nil, false, true},
		{"unknown", domain.AssessmentFailed, []string{"unknown"}, false, true},
		{"declined enabled", domain.AssessmentDeclined, nil, true, true},
		{"declined disabled", domain.AssessmentDeclined, nil, false, false},
		{"completed", domain.AssessmentCompleted, nil, true, false},
		{"interrupted", domain.AssessmentInterrupted, nil, true, false},
	} {
		t.Run(
			test.name,
			func(t *testing.T) {
				outcome := domain.AssessmentOutcome{
					Disposition:    test.disposition,
					FailureClasses: test.classes,
				}
				outcome.SetRetryable(test.declined)
				decoded := domain.DecodeAssessmentOutcome(domain.EncodeAssessmentOutcome(outcome))
				if decoded.Retryable == nil || *decoded.Retryable != test.want {
					t.Fatalf(
						"The decoded retryable field is %v. The expected retry eligibility is %v.",
						decoded.Retryable,
						test.want,
					)
				}
			},
		)
	}

	legacy := domain.AssessmentOutcome{
		Disposition:    domain.AssessmentFailed,
		FailureClasses: []string{"panic"},
	}
	decoded := domain.DecodeAssessmentOutcome(domain.EncodeAssessmentOutcome(legacy))
	if decoded.Retryable != nil || !decoded.RetryEligibility(false) {
		t.Fatalf("legacy failure lost its previous retry behavior: %+v", decoded)
	}
}

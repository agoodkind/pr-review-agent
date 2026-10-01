package review

import (
	"errors"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
)

type usageFailure struct{}

func (usageFailure) Error() string       { return "usage exhausted" }
func (usageFailure) UsageExceeded() bool { return true }

type unavailableFailure struct{}

func (unavailableFailure) Error() string             { return "provider unavailable" }
func (unavailableFailure) ProviderUnavailable() bool { return true }

func TestAMixedFailurePassesOnlyWhenEveryClassDoes(t *testing.T) {
	service := &Service{failureAppearances: config.FailureAppearances{
		config.FailureUsageExceeded: config.FailureAppearancePass,
	}}
	joined := errors.Join(usageFailure{}, unavailableFailure{})

	if got := service.presentedConclusion(failureClassesOf(joined), "failure"); got != "failure" {
		t.Fatalf("joined conclusion = %q, want failure", got)
	}
	if got := service.presentedConclusion(failureClassesOf(usageFailure{}), "failure"); got != "success" {
		t.Fatalf("usage conclusion = %q, want success", got)
	}
	unread := []chunkFailure{{chunk: 1, err: usageFailure{}}, {chunk: 2, err: unavailableFailure{}}}
	if got := service.presentedConclusion(chunkFailureClasses(unread), "action_required"); got != "action_required" {
		t.Fatalf("unread conclusion = %q, want action_required", got)
	}
}

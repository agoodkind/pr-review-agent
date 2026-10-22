package review

import (
	"context"
	"errors"

	"goodkind.io/pr-review-agent/internal/config"
)

type scopeFailureState struct {
	cause    error
	statuses []ProviderStatus
	classes  []config.FailureClass
}

func (pass *chunkPass) recordAssessmentFailure(err error) {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	pass.assessmentErr = err
	pass.coverage = false
}

func (pass *chunkPass) scopeFailures(ctx context.Context) scopeFailureState {
	failures := pass.unreadChunks()
	pass.mu.Lock()
	assessmentErr := pass.assessmentErr
	pass.mu.Unlock()
	state := scopeFailureState{
		cause:    errors.Join(joinedChunkFailures(ctx, failures), assessmentErr),
		statuses: providerStatuses(failures), classes: nil,
	}
	collectProviderStatuses(assessmentErr, &state.statuses)
	if state.cause != nil {
		state.classes = failureClassesOf(state.cause)
	}
	return state
}

func (state scopeFailureState) classNames() []string {
	names := make([]string, 0, len(state.classes))
	for _, class := range state.classes {
		names = append(names, string(class))
	}
	return names
}

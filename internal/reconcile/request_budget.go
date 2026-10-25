package reconcile

import (
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// RequestSizer measures the complete provider request for reconciliation.
type RequestSizer interface {
	ReconcileRequestBytes(string) (int, error)
}

// ThreadInput contains the required evidence for one reconciliation decision.
type ThreadInput struct {
	ID   string
	Text string
}

// BatchPrompt contains a complete prompt and the threads the request evaluates.
type BatchPrompt struct {
	ThreadIDs []string
	Prompt    string
}

// BuildReconciliationRequests fits required thread evidence within complete native requests.
// A nil sizer measures the rendered prompt for models without native request sizing.
func BuildReconciliationRequests(threads []ThreadInput, pullRequest githubapp.PullRequest, policy reviewrules.Policy, sizer RequestSizer, maximumBytes int) ([]BatchPrompt, error) {
	prepared := make([]preparedThread, 0, len(threads))
	for _, thread := range threads {
		if thread.ID == "" {
			return nil, errors.New("reconciliation request requires a thread ID")
		}
		var owned domain.OwnedThread
		owned.NodeID = thread.ID
		prepared = append(prepared, preparedThread{thread: owned, text: thread.Text})
	}
	batches, budgetErr := boundThreadBatches(sizer, prepared, pullRequest, policy, maximumBytes)
	requests := make([]BatchPrompt, 0, len(batches))
	for index, batch := range batches {
		prompt, err := buildBatchPrompt(batch, index+1, len(batches), pullRequest, policy)
		if err != nil {
			return nil, err
		}
		requests = append(requests, BatchPrompt{ThreadIDs: preparedThreadIDs(batch), Prompt: prompt})
	}
	return requests, budgetErr
}

type preparedBatch struct {
	threads []preparedThread
	prompt  string
}

func boundPreparedThreads(model Model, threads []preparedThread, pullRequest githubapp.PullRequest, policy reviewrules.Policy, maximumBytes int) ([]preparedBatch, error) {
	inputs := make([]ThreadInput, 0, len(threads))
	byID := make(map[string]preparedThread, len(threads))
	for _, thread := range threads {
		inputs = append(inputs, ThreadInput{ID: thread.thread.NodeID, Text: thread.text})
		byID[thread.thread.NodeID] = thread
	}
	sizer, _ := model.(RequestSizer)
	requests, err := BuildReconciliationRequests(inputs, pullRequest, policy, sizer, maximumBytes)
	batches := make([]preparedBatch, 0, len(requests))
	for _, request := range requests {
		batch := preparedBatch{threads: make([]preparedThread, 0, len(request.ThreadIDs)), prompt: request.Prompt}
		for _, id := range request.ThreadIDs {
			batch.threads = append(batch.threads, byID[id])
		}
		batches = append(batches, batch)
	}
	return batches, err
}

func boundThreadBatches(sizer RequestSizer, threads []preparedThread, pullRequest githubapp.PullRequest, policy reviewrules.Policy, maximumBytes int) ([][]preparedThread, error) {
	if maximumBytes < 1 {
		return nil, errors.New("reconciliation request budget must be positive")
	}
	batches := batchPreparedThreads(threads, maximumBytes)
	var failures []error
	for index := 0; index < len(batches); {
		actual, err := reconciliationBatchBytes(sizer, batches[index], index+1, len(batches), pullRequest, policy)
		if err != nil {
			return nil, err
		}
		if actual <= maximumBytes {
			index++
			continue
		}
		batch := batches[index]
		if len(batch) == 1 {
			failure := fmt.Errorf("reconciliation thread %s: %w", batch[0].thread.NodeID, &diff.PromptSizeError{Actual: actual, Limit: maximumBytes})
			failures = append(failures, failure)
			batches = append(batches[:index], batches[index+1:]...)
		} else {
			middle := len(batch) / 2
			replacement := [][]preparedThread{batch[:middle], batch[middle:]}
			replacement = append(replacement, batches[index+1:]...)
			batches = append(batches[:index], replacement...)
		}
		// The final batch count can change the configured prompt at every index.
		index = 0
	}
	if err := errors.Join(failures...); err != nil {
		slog.Warn("Reconciliation threads exceed the request budget", "err", err)
		return batches, err
	}
	return batches, nil
}

func reconciliationBatchBytes(sizer RequestSizer, batch []preparedThread, index, total int, pullRequest githubapp.PullRequest, policy reviewrules.Policy) (int, error) {
	prompt, err := buildBatchPrompt(batch, index, total, pullRequest, policy)
	if err != nil {
		return 0, err
	}
	if sizer != nil {
		actual, err := sizer.ReconcileRequestBytes(prompt)
		if err != nil {
			slog.Warn("Reconciliation request measurement failed", "err", err)
			return 0, fmt.Errorf("measure reconciliation request: %w", err)
		}
		return actual, nil
	}
	return len(prompt), nil
}

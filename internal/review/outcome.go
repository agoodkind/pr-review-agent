package review

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
)

// OutcomeSink must persist retry intent before a check becomes terminal.
type OutcomeSink func(context.Context, domain.ReviewJob, domain.AssessmentOutcome) (domain.AssessmentOutcome, error)

type (
	outcomeKey     struct{}
	outcomeSinkKey struct{}
)

type outcomeRecorder struct {
	persisted bool
	job       domain.ReviewJob
	outcome   domain.AssessmentOutcome
}

// WithOutcomeSink applies persistence to ordinary reviews and automatic retries.
func WithOutcomeSink(ctx context.Context, sink OutcomeSink) context.Context {
	return context.WithValue(ctx, outcomeSinkKey{}, sink)
}

func recordAssessment(ctx context.Context, disposition domain.AssessmentDisposition, summary *Summary, classes []string, availableAt int64) {
	recorder, ok := ctx.Value(outcomeKey{}).(*outcomeRecorder)
	if !ok {
		return
	}
	recorder.outcome = domain.AssessmentOutcome{
		Nonce:       rand.Text(),
		Disposition: disposition, Head: recorder.job.Head,
		MetadataRevision: "", CoverageComplete: false, ReassessmentID: "", RetryAtMS: 0,
		FailureClasses: classes, AvailableAtMS: availableAt, CheckRunID: recorder.job.CheckRunID,
	}
	if summary != nil {
		recorder.outcome.MetadataRevision = summary.MetadataRevision
		recorder.outcome.CoverageComplete = summary.CoverageComplete
	}
	recorder.persisted = false
}

func failureNames(cause error) []string {
	classes := failureClassesOf(cause)
	names := make([]string, 0, len(classes))
	for _, class := range classes {
		names = append(names, string(class))
	}
	return names
}

func quotaRecovery(statuses []ProviderStatus) int64 {
	earliest := int64(0)
	for _, status := range statuses {
		if status.Cause != ProviderAppBudgetDenied || status.QuotaSnapshot.AvailableAtMS == 0 {
			return 0
		}
		at := status.QuotaSnapshot.AvailableAtMS
		if earliest == 0 || at < earliest {
			earliest = at
		}
	}
	return earliest
}

func persistAssessment(ctx context.Context, checkID int64) (domain.AssessmentOutcome, error) {
	recorder, ok := ctx.Value(outcomeKey{}).(*outcomeRecorder)
	if !ok {
		var empty domain.AssessmentOutcome
		return empty, nil
	}
	recorder.outcome.CheckRunID = checkID
	if recorder.persisted {
		return recorder.outcome, nil
	}
	sink, ok := ctx.Value(outcomeSinkKey{}).(OutcomeSink)
	if ok {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		outcome, err := sink(persistCtx, recorder.job, recorder.outcome)
		if err != nil {
			return recorder.outcome, err
		}
		recorder.outcome = outcome
	}
	recorder.persisted = true
	return recorder.outcome, nil
}

func reassessmentNotice(ctx context.Context) string {
	recorder, ok := ctx.Value(outcomeKey{}).(*outcomeRecorder)
	if !ok || recorder.outcome.ReassessmentID == "" {
		return ""
	}
	return "\n\n<!-- pr-review-agent:reassessment:start -->\nThe queue will reassess this pull request after `" + time.UnixMilli(recorder.outcome.RetryAtMS).UTC().Format(time.RFC3339) + "`. The reassessment identifier is `" + recorder.outcome.ReassessmentID + "`.\n<!-- pr-review-agent:reassessment:end -->"
}

func stripReassessmentNotice(body string) string {
	const start = "<!-- pr-review-agent:reassessment:start -->"
	const end = "<!-- pr-review-agent:reassessment:end -->"
	first := strings.Index(body, start)
	if first < 0 {
		return body
	}
	last := strings.Index(body[first:], end)
	if last < 0 {
		return body
	}
	return body[:first] + body[first+last+len(end):]
}

// Run preserves the existing runner contract for callers that need only errors.
func (service *Service) Run(parent context.Context, job domain.ReviewJob) error {
	_, err := service.RunWithOutcome(parent, job)
	return err
}

// RunWithOutcome reports incomplete coverage separately from successful checks.
func (service *Service) RunWithOutcome(parent context.Context, job domain.ReviewJob) (domain.AssessmentOutcome, error) {
	recorder := &outcomeRecorder{persisted: false, job: job, outcome: domain.AssessmentOutcome{Nonce: "", Disposition: domain.AssessmentDeclined, Head: job.Head, CheckRunID: job.CheckRunID, MetadataRevision: "", FailureClasses: nil, AvailableAtMS: 0, CoverageComplete: false, ReassessmentID: "", RetryAtMS: 0}}
	ctx := context.WithValue(parent, outcomeKey{}, recorder)
	err := service.run(ctx, job)
	if err != nil && recorder.outcome.Disposition == domain.AssessmentDeclined {
		disposition := domain.AssessmentFailed
		if errors.Is(err, context.Canceled) && parent.Err() != nil {
			disposition = domain.AssessmentInterrupted
		}
		recorder.outcome.Disposition = disposition
		recorder.outcome.FailureClasses = failureNames(err)
	}
	return recorder.outcome, err
}

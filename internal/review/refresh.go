package review

// This file reconciles the standing verdict with thread state at a head that is
// already reviewed.
//
// It is what lets resolving a thread unblock a pull request with no push. A
// pull_request_review_thread delivery arrives at a head some earlier run
// already judged, so there is no delta to review and nothing to ask a model:
// the verdict is a pure function of the service's own open threads and how much
// of the head was read, and both are already known.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// emptyReview is the zero verdict returned beside a failure or a miss.
func emptyReview() githubapp.Review {
	return githubapp.Review{ID: 0, CommitID: "", Author: "", Body: "", State: ""}
}

// refreshVerdictAtReviewedHead reconciles the standing verdict with current
// thread state at a head that is already reviewed.
//
// Every failure travels back to the caller rather than being logged and dropped
// here. A refresh that failed silently was indistinguishable from one that found
// nothing to do, so a resolved thread could keep blocking the pull request with
// nothing saying why.
//
// Returning it does not by itself retry anything: the webhook is acknowledged
// before this runs, and the dispatcher logs a failed job rather than replaying
// it. What this buys is that the failure is attributed to the run and carries
// its cause. The caller completes the visible check before returning this, so
// the head still reads as reviewed.
//
// reviews is the review list the caller already loaded, or nil when the run
// exited before loading one.
func (service *Service) refreshVerdictAtReviewedHead(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
) error {
	ctx, cancel := service.publicationContext(ctx)
	defer cancel()

	inputs, err := service.loadVerdictRefreshInputs(ctx, job, reviews)
	if err != nil || !inputs.found {
		return err
	}
	// How much of the head was reviewed is recovered from the review that named
	// this head, because that is the run that knew. What the pull request
	// currently shows is a separate question, answered by the newest verdict
	// whatever head it named. A thread resolution changes neither.
	headFullyReviewed := !strings.Contains(inputs.verdict.Body, unreviewedHeadReason)
	return service.applyRefreshedVerdict(ctx, job, refreshedVerdict{
		decision:          reviewerDecision(inputs.threads, service.botLogin, headFullyReviewed),
		standingState:     inputs.standingState,
		threads:           inputs.threads,
		headFullyReviewed: headFullyReviewed,
		blockWithdrawn:    inputs.withdrawn,
	})
}

// verdictRefreshInputs is everything a refresh decides from.
type verdictRefreshInputs struct {
	// verdict is the review that named this head, which is where how much was
	// reviewed is recovered from. A dismissed review serves that just as well,
	// because dismissing one does not edit its body.
	verdict githubapp.Review
	// standingState is what GitHub shows for this service now, across all heads.
	standingState string
	threads       []githubapp.ReviewThread
	found         bool
	// withdrawn is whether a person dismissed the verdict this head carried.
	withdrawn bool
}

// loadVerdictRefreshInputs reads the standing verdict and the current threads a
// refresh decides from. It reports found false, with no error, when this head
// carries no verdict of the service's own to reconcile.
func (service *Service) loadVerdictRefreshInputs(
	ctx context.Context,
	job domain.ReviewJob,
	reviews []githubapp.Review,
) (verdictRefreshInputs, error) {
	logger := gklog.L(ctx)
	missing := verdictRefreshInputs{
		verdict: emptyReview(), standingState: "", threads: nil, found: false, withdrawn: false,
	}
	if reviews == nil {
		listed, err := service.github.ListReviews(ctx, job.InstallationID, job.Repository, job.Number)
		if err != nil {
			logger.ErrorContext(ctx, "list reviews for verdict refresh", slog.String("err", err.Error()))
			return missing, fmt.Errorf("list reviews for verdict refresh: %w", err)
		}
		reviews = listed
	}
	verdict := latestBotVerdictAtHead(reviews, service.botLogin, job.Head)
	if !verdict.found {
		return missing, nil
	}
	threads, err := service.github.ListReviewThreads(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "list threads for verdict refresh", slog.String("err", err.Error()))
		return missing, fmt.Errorf("list threads for verdict refresh: %w", err)
	}
	return verdictRefreshInputs{
		verdict:       verdict.review,
		standingState: latestBotVerdictState(reviews, service.botLogin),
		threads:       threads,
		found:         true,
		withdrawn:     verdict.withdrawn,
	}, nil
}

// refreshedVerdict is what one refresh computed from current thread state.
type refreshedVerdict struct {
	decision          domain.ReviewDecision
	standingState     string
	threads           []githubapp.ReviewThread
	headFullyReviewed bool
	// blockWithdrawn is whether a person dismissed the verdict at this head,
	// which bounds what the refresh may submit rather than what it computes.
	blockWithdrawn bool
}

// mayPublish reports whether the refresh may submit the verdict it computed.
//
// A refresh never reinstates a block a person withdrew. Dismissing is how
// somebody says they do not want this verdict holding the pull request, and a
// service that submitted the same block again from thread state alone would
// resurrect by machinery, seconds later and with no new information, the stale
// block this project exists to kill.
//
// Approving is still allowed, and is the reason a dismissed head is refreshed at
// all. Once every thread is resolved the recomputed verdict is an approval,
// which is what the person was reaching for, and the refresh is the only path
// that reaches it without a push.
//
// Otherwise a verdict matching what GitHub already shows is not submitted,
// because a second identical verdict is noise.
func (refreshed refreshedVerdict) mayPublish() bool {
	if refreshed.blockWithdrawn && refreshed.decision != domain.ReviewDecisionApprove {
		return false
	}
	return reviewStateFor(refreshed.decision) != refreshed.standingState
}

// applyRefreshedVerdict writes what the refresh computed: a fresh verdict review
// when the decision moved, and the summary comment prose either way.
//
// The summary is rewritten even when the decision did not move. The blocking
// list carries one entry per open thread, so resolving one of several leaves the
// verdict identical while the list still names the thread that just closed, and
// a reader acting on that list goes looking for something already dealt with.
// The durable state is kept exactly as the last completed run wrote it.
func (service *Service) applyRefreshedVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	refreshed refreshedVerdict,
) error {
	logger := gklog.L(ctx)
	// A verdict written at a moved head would judge a commit nobody is looking
	// at any more, so the refresh checks the head one last time.
	currentPullRequest, err := service.github.GetPullRequest(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "refresh verdict head check", slog.String("err", err.Error()))
		return fmt.Errorf("refresh verdict head check: %w", err)
	}
	if currentPullRequest.Head != job.Head {
		logger.InfoContext(ctx, "verdict refresh skipped", slog.String("reason", "head_moved"))
		return nil
	}

	summary := Summary{
		Head:     job.Head,
		Decision: refreshed.decision,
		Blocking: blockingReasons(
			refreshed.threads, service.botLogin, job.PullRequestRef, refreshed.headFullyReviewed,
		),
		Models:            nil,
		Duration:          0,
		FilesReviewed:     0,
		Chunks:            0,
		CoverageComplete:  refreshed.headFullyReviewed,
		MinimumImportance: service.minimumImportance,
		Observed:          nil,
		Eligible:          nil,
		Published:         nil,
		PriorReviews:      nil,
		Threads:           traceThreads(refreshed.threads, service.botLogin),
		Reached:           "",
		Failed:            false,
		// A refresh runs only at a head some earlier run already reviewed, which
		// is a gate a forced run never reaches.
		Forced: false,
	}
	if refreshed.mayPublish() {
		if err := service.submitRefreshedVerdict(ctx, job, summary); err != nil {
			return err
		}
	} else if refreshed.blockWithdrawn {
		logger.InfoContext(
			ctx,
			"verdict refresh withheld",
			slog.String("reason", "block_withdrawn_by_hand"),
			slog.String("decision", string(refreshed.decision)),
		)
	}
	if err := service.upsertSummaryCommentFrom(ctx, job, func(state marker.State) summaryCommentContent {
		return summaryCommentContent{
			Prose: renderVerdictRefreshProse(summary, refreshed.blockWithdrawn),
			State: state,
		}
	}); err != nil {
		logger.ErrorContext(ctx, "update summary after verdict refresh", slog.String("err", err.Error()))
		return fmt.Errorf("update summary after verdict refresh: %w", err)
	}
	return nil
}

// submitRefreshedVerdict publishes a fresh verdict review at the reviewed head.
func (service *Service) submitRefreshedVerdict(
	ctx context.Context,
	job domain.ReviewJob,
	summary Summary,
) error {
	logger := gklog.L(ctx)
	submitted, err := service.github.SubmitReview(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
		githubapp.SubmitReviewRequest{
			CommitID: job.Head,
			Body:     RenderVerdictBody(summary),
			Event:    summary.Decision,
			Comments: nil,
		},
	)
	if err != nil {
		logger.ErrorContext(ctx, "submit refreshed verdict", slog.String("err", err.Error()))
		return fmt.Errorf("submit refreshed verdict: %w", err)
	}
	logger.InfoContext(
		ctx,
		"review verdict refreshed",
		slog.Int64("review_id", submitted.ID),
		slog.String("event", string(summary.Decision)),
		slog.Any("blocking", summary.Blocking),
	)
	return nil
}

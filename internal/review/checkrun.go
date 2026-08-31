package review

// This file owns admission: which check run a delivery reports through, and
// whether the delivery is admitted at all.
//
// Two failures meet here and pull in opposite directions. Admitting one force
// request twice pays for the whole analysis again and publishes a duplicate
// review over the first. Refusing one that was never actually taken on leaves
// the pull request carrying a check nobody will ever clear, with no way to ask
// again, which is the stale block this service exists to prevent. The check run
// is what tells them apart, and its state rather than its existence is what
// carries the answer.

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

const (
	// checkRunQueued is the state CreateCheckRun leaves a new check run in, and
	// the state StartCheckRun moves it out of. A check run this service created
	// and left queued is one whose start never landed, so no work was ever
	// accepted for it.
	checkRunQueued = "queued"
	// checkRunInProgress is the state StartCheckRun sets, and what a running
	// review reports until it concludes.
	checkRunInProgress = "in_progress"
)

// forcedAdmission is what an earlier arrival of this same delivery left behind
// on GitHub.
type forcedAdmission struct {
	checkRun githubapp.CheckRun
	// found is whether this delivery already created a check run at this head.
	found bool
	// accepted is whether that check run was started, which is what says the
	// force was taken on rather than abandoned before any work began.
	accepted bool
}

// emptyForcedAdmission is the answer for a delivery that left nothing behind.
func emptyForcedAdmission() forcedAdmission {
	return forcedAdmission{
		checkRun: githubapp.CheckRun{
			ID: 0, Name: "", Head: "", Status: "", Conclusion: "", ExternalID: "",
		},
		found:    false,
		accepted: false,
	}
}

// priorForcedAdmission reports what an earlier arrival of this forced delivery
// left behind, and whether it got far enough to count as taken on.
//
// A check run existing is not proof that the work was accepted, because it is
// created before it is started. A create that lands and a start that fails
// leaves a check run this delivery owns, queued, with nothing running. Refusing
// the redelivery on the strength of that check run existing would strand the
// pull request on a check that can never conclude, so the predicate is the
// state and not the existence.
//
// The states are the ones this service itself writes, not names read off a
// field: CreateCheckRun sends queued, StartCheckRun sends in_progress, and
// CompleteCheckRun sends completed. The check run looked up here is always one
// this service created, so queued means the start never landed.
func (service *Service) priorForcedAdmission(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
) (forcedAdmission, error) {
	logger := gklog.L(ctx)
	if !job.Forced {
		return emptyForcedAdmission(), nil
	}
	checkRun, found, err := service.github.FindCheckRunByExternalID(
		ctx,
		job.InstallationID,
		job.Repository,
		head,
		service.checkName,
		job.DeliveryID,
	)
	if err != nil {
		logger.ErrorContext(ctx, "find check run by delivery", slog.String("err", err.Error()))
		return emptyForcedAdmission(), fmt.Errorf("find check run by delivery: %w", err)
	}
	if !found {
		return emptyForcedAdmission(), nil
	}
	if checkRun.Status == checkRunQueued {
		logger.InfoContext(
			ctx,
			"resuming a forced review whose check never started",
			slog.Int64("check_run_id", checkRun.ID),
		)
	}
	return forcedAdmission{
		checkRun: checkRun,
		found:    true,
		accepted: checkRun.Status != checkRunQueued,
	}, nil
}

// ensureCheckRun returns the check run this job reports through, and whether
// the job was admitted at all.
//
// A force request is admitted once per delivery. GitHub reuses a delivery
// identifier when it redelivers, and the replay queue replays the delivery a
// container could not take, so the same force request can arrive more than
// once. The check run this delivery already created is what records that, and
// it has to be, because the in process delivery cache cannot answer here: the
// forced path destroys the container, so by the time a redelivery arrives that
// cache is a fresh empty one. A second label event carries its own delivery
// identifier and is a genuinely new force request, which is why the identifier
// rather than the fact of being forced is the key.
//
// A redelivery whose earlier attempt never started its check run is resumed
// rather than refused: it reuses that check run instead of creating a second,
// starts it, and is admitted, so the work the first attempt never accepted
// happens now.
func (service *Service) ensureCheckRun(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
) (githubapp.CheckRun, bool, error) {
	logger := gklog.L(ctx)
	prior, err := service.priorForcedAdmission(ctx, job, head)
	if err != nil {
		return githubapp.CheckRun{}, false, err
	}
	if prior.found && prior.accepted {
		logger.InfoContext(
			ctx,
			"review job suppressed",
			slog.String("reason", "duplicate_force"),
			slog.Int64("check_run_id", prior.checkRun.ID),
		)
		return prior.checkRun, false, nil
	}

	checkRun := prior.checkRun
	if !prior.found {
		checkRun, err = service.checkRunForHead(ctx, job, head)
		if err != nil {
			return githubapp.CheckRun{}, false, err
		}
	}
	if checkRun.Status == checkRunQueued {
		if err := service.github.StartCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			checkRun.ID,
			repositoryURL(job.Repository),
		); err != nil {
			logger.ErrorContext(ctx, "start check run", slog.String("err", err.Error()))
			return githubapp.CheckRun{}, false, fmt.Errorf("start check run: %w", err)
		}
		checkRun.Status = checkRunInProgress
	}
	return checkRun, true, nil
}

// checkRunForHead returns the check run this job reports through when the
// delivery left none behind: the one the head already carries, or a new one.
//
// A forced run always gets a new one rather than inheriting what the head
// carries. That check run is completed, and a completed successful check
// satisfies branch protection, so inheriting it would leave the pull request
// mergeable for the whole forced run and let the change the label was added to
// re-examine merge on the strength of the verdict being replaced.
func (service *Service) checkRunForHead(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
) (githubapp.CheckRun, error) {
	logger := gklog.L(ctx)
	checkRun, found, err := service.github.FindCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		head,
		service.checkName,
	)
	if err != nil {
		logger.ErrorContext(ctx, "find check run", slog.String("err", err.Error()))
		return githubapp.CheckRun{}, fmt.Errorf("find check run: %w", err)
	}
	if found && !job.Forced {
		return checkRun, nil
	}
	created, err := service.github.CreateCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		head,
		service.checkName,
		job.DeliveryID,
	)
	if err != nil {
		logger.ErrorContext(ctx, "create check run", slog.String("err", err.Error()))
		return githubapp.CheckRun{}, fmt.Errorf("create check run: %w", err)
	}
	return created, nil
}

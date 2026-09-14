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
	// checkRunCompleted is the state CompleteCheckRun sets. Its conclusion says
	// whether the delivery finished or was cancelled before it completed.
	checkRunCompleted = "completed"
	// checkConclusionCancelled marks a delivery that stopped before it carried
	// its requested work through. A redelivery may resume that check.
	checkConclusionCancelled = "cancelled"
	// checkConclusionSuccess is the one conclusion that says this head's review
	// completed. The verdict says whether the review approved or requested
	// changes. It is named here rather than written out at each reader because a
	// run that reports through a check and a run that stops on one have to mean
	// the same thing by it.
	checkConclusionSuccess = "success"
)

// needsStarting reports whether a check has to be moved to in progress before a
// review reports through it.
//
// A queued check has never been started. A completed one has been started and
// concluded, and a review reporting through that would leave a terminal
// conclusion standing on the head for its whole duration, with nothing moving it
// back: a reader sees a concluded review while one is running, and every stage
// that reports progress writes into a check that already said it was finished.
//
// A completed successful check is the exception, and deliberately so. An
// ordinary run stops on it rather than reviewing, so it has nothing to report
// through it. Forced runs and verdict refreshes use dedicated checks instead.
func needsStarting(checkRun githubapp.CheckRun) bool {
	if checkRun.Status == checkRunQueued {
		return true
	}
	if checkRun.Status != checkRunCompleted {
		return false
	}
	return checkRun.Conclusion != checkConclusionSuccess
}

// dedicatedAdmission is what an earlier arrival of this same delivery left
// behind on GitHub.
type dedicatedAdmission struct {
	checkRun githubapp.CheckRun
	// found is whether this delivery already created a check run at this head.
	found bool
	// visible is whether this check is the newest one of this name on the head.
	// Only that check represents the current result in GitHub's rollup.
	visible bool
	// settled is whether that check run reached a non-cancelled terminal state,
	// which is the only evidence available here that the delivery was carried
	// through.
	//
	// Nothing weaker will do. A check run is created before it is started and
	// started before the review runs, and a process that died anywhere in that
	// window leaves one behind that no longer has anything driving it. Reading
	// such a check run as proof the work was taken would leave the pull request
	// on a check that can never conclude, with no way to ask again, which is the
	// stale block this service exists to prevent.
	//
	// The service has no durable record of acceptance to consult instead: its
	// review queue is in memory and dies with the container. A non-cancelled
	// terminal state is therefore the strongest true signal. Everything short
	// of it is resumed.
	settled bool
}

// emptyDedicatedAdmission is the answer for a delivery that left nothing behind.
func emptyDedicatedAdmission() dedicatedAdmission {
	return dedicatedAdmission{
		checkRun: githubapp.CheckRun{
			ID: 0, Name: "", Head: "", Status: "", Conclusion: "", ExternalID: "",
		},
		found:   false,
		visible: false,
		settled: false,
	}
}

// priorDedicatedAdmission reports what an earlier arrival of this forced or
// verdict refresh delivery left behind, and whether it was carried through.
//
// A check run existing is not proof of anything except that a create landed. It
// is created before it is started and started before the review runs, so a
// process that died anywhere in that window leaves one behind with nothing
// driving it. The predicate is therefore the state and not the existence.
//
// The states are the ones this service itself writes, not names read off a
// field: CreateCheckRun sends queued, StartCheckRun sends in_progress, and
// CompleteCheckRun sends completed. A cancelled conclusion says the run did
// not finish its requested work. Every other completed conclusion says it did.
//
// A review that is genuinely still running is not reached here at all. The
// handler claims each delivery identifier in the delivery cache before
// admission, so a redelivery arriving at the process that is running the review
// is answered from that claim and never reaches this code. Anything that does
// reach it comes from a process that no longer holds the claim, which is to say
// a process that is gone.
func (service *Service) priorDedicatedAdmission(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
) (dedicatedAdmission, error) {
	logger := gklog.L(ctx)
	if !job.Forced && !job.RefreshVerdict {
		return emptyDedicatedAdmission(), nil
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
		return emptyDedicatedAdmission(), fmt.Errorf("find check run by delivery: %w", err)
	}
	if !found {
		return emptyDedicatedAdmission(), nil
	}
	settled := checkRun.Status == checkRunCompleted &&
		checkRun.Conclusion != checkConclusionCancelled
	visible := false
	if !settled {
		latest, latestFound, findErr := service.github.FindCheckRun(
			ctx,
			job.InstallationID,
			job.Repository,
			head,
			service.checkName,
		)
		if findErr != nil {
			logger.ErrorContext(ctx, "find current check run", slog.String("err", findErr.Error()))
			return emptyDedicatedAdmission(), fmt.Errorf("find current check run: %w", findErr)
		}
		visible = latestFound && latest.ID == checkRun.ID
		if latestFound && latest.ExternalID == job.DeliveryID {
			checkRun = latest
			visible = true
			settled = checkRun.Status == checkRunCompleted &&
				checkRun.Conclusion != checkConclusionCancelled
		}
	}
	if !settled {
		logger.InfoContext(
			ctx,
			"resuming a review delivery that never finished",
			slog.Int64("check_run_id", checkRun.ID),
			slog.String("status", checkRun.Status),
			slog.Bool("visible", visible),
		)
	}
	return dedicatedAdmission{
		checkRun: checkRun,
		found:    true,
		visible:  visible,
		settled:  settled,
	}, nil
}

// ensureCheckRun returns the check run this job reports through, and whether
// the job was admitted at all.
//
// Forced runs and verdict refreshes are admitted once per delivery. GitHub
// reuses a delivery identifier when it redelivers, and the replay queue replays
// a delivery a container could not take. The check run this delivery already
// created records that across process restarts. A distinct event carries a
// distinct delivery identifier, so it owns a distinct check.
//
// A redelivery whose earlier attempt did not finish is resumed rather than
// refused: it reuses that check run instead of creating a second, starts it if
// the start never landed, and is admitted, so the caller enqueues the work the
// earlier attempt never completed. A completed cancellation is also resumed,
// because it says the admitted work never reached the queue or stopped before
// it completed. Other terminal states say the delivery was carried through.
func (service *Service) ensureCheckRun(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
) (githubapp.CheckRun, bool, error) {
	logger := gklog.L(ctx)
	prior, err := service.priorDedicatedAdmission(ctx, job, head)
	if err != nil {
		return githubapp.CheckRun{}, false, err
	}
	if prior.found && prior.settled {
		logger.InfoContext(
			ctx,
			"review job suppressed",
			slog.String("reason", "duplicate_delivery"),
			slog.Int64("check_run_id", prior.checkRun.ID),
		)
		return prior.checkRun, false, nil
	}

	checkRun := prior.checkRun
	if !prior.found || !prior.visible {
		checkRun, err = service.checkRunForHead(ctx, job, head)
		if err != nil {
			return githubapp.CheckRun{}, false, err
		}
	}
	if needsStarting(checkRun) {
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
		// The start clears the conclusion on GitHub, so the value carried on is
		// the one the check now has rather than the one it is leaving behind.
		checkRun.Conclusion = ""
	}
	return checkRun, true, nil
}

// checkRunForHead returns the check run this job reports through when the
// delivery left none behind: the one the head already carries, or a new one.
//
// Forced runs and verdict refreshes always get a new check. A forced run cannot
// inherit a passing check while it replaces that verdict. Each refresh needs
// its own check because several refresh deliveries for one pull request may
// wait in the queue. One delivery completing must not make a later delivery
// appear finished before it runs.
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
	if found && !job.Forced && !job.RefreshVerdict {
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

// succeed concludes a check run as passing, with the run summary the reader
// sees on the pull request.
func (service *Service) succeed(
	ctx context.Context,
	job domain.ReviewJob,
	checkRunID int64,
	title string,
	summary string,
) error {
	logger := gklog.L(ctx)
	if err := service.completeCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		checkRunID,
		"success",
		title,
		summary,
	); err != nil {
		logger.ErrorContext(ctx, "complete successful check run", slog.String("err", err.Error()))
		return fmt.Errorf("complete check run: %w", err)
	}
	return nil
}

// cancelCheck concludes a check run the run abandoned, which is what a head
// that moved mid review leaves behind.
func (service *Service) cancelCheck(ctx context.Context, job domain.ReviewJob, checkRunID int64) error {
	logger := gklog.L(ctx)
	if err := service.completeCheckRun(
		ctx,
		job.InstallationID,
		job.Repository,
		checkRunID,
		checkConclusionCancelled,
		checkSummaryCancelled,
		checkSummaryCancelled,
	); err != nil {
		logger.ErrorContext(ctx, "complete cancelled check run", slog.String("err", err.Error()))
		return fmt.Errorf("complete cancelled check run: %w", err)
	}
	return nil
}

// completeCheckRun writes one terminal conclusion, under its own timeout so a
// cancelled review still records how it ended.
func (service *Service) completeCheckRun(
	ctx context.Context,
	installationID int64,
	repository domain.Repository,
	checkRunID int64,
	conclusion string,
	title string,
	summary string,
) error {
	logger := gklog.L(ctx)
	completionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), service.checkCompletionTimeout)
	defer cancel()
	// The log is rendered before the completion call, so the published text is
	// everything the run recorded up to the moment it finished.
	err := service.github.CompleteCheckRun(
		completionCtx,
		installationID,
		repository,
		checkRunID,
		conclusion,
		title,
		summary,
		renderRunLog(ctx),
	)
	if err != nil {
		logger.ErrorContext(ctx, "complete check run", slog.String("err", err.Error()))
		return fmt.Errorf("complete check run: %w", err)
	}
	return nil
}

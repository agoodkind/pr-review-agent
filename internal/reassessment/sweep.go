package reassessment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

type inventoryStage int

const (
	inventoryInitial inventoryStage = iota
	inventoryInstallations
	inventoryRepositories
	inventoryPullRequests
	inventoryTargets
)

// InventoryGitHub requires validated page boundaries before cursor advancement.
type InventoryGitHub interface {
	ListInstallationsPage(context.Context, int, int) ([]githubapp.Installation, bool, error)
	ListInstallationRepositoriesPage(context.Context, int64, int, int) ([]domain.Repository, bool, error)
	ListOpenPullRequestsPage(context.Context, int64, domain.Repository, int, int) ([]githubapp.PullRequest, bool, error)
}

// SweepCursor resumes bounded installation inventory without restarting earlier pages.
type SweepCursor struct {
	Version           int64                   `json:"version"`
	Stage             inventoryStage          `json:"stage"`
	InstallationPage  int                     `json:"installation_page"`
	InstallationIndex int                     `json:"installation_index"`
	InstallationIDs   []int64                 `json:"installation_ids"`
	InstallationsNext bool                    `json:"installations_next"`
	RepositoryPage    int                     `json:"repository_page"`
	RepositoryIndex   int                     `json:"repository_index"`
	Repositories      []domain.Repository     `json:"repositories"`
	RepositoriesNext  bool                    `json:"repositories_next"`
	PullRequestPage   int                     `json:"pull_request_page"`
	PullRequestIndex  int                     `json:"pull_request_index"`
	PullRequests      []domain.PullRequestRef `json:"pull_requests"`
	PullRequestsNext  bool                    `json:"pull_requests_next"`
	NotBeforeMS       int64                   `json:"not_before_ms"`
}

// InventoryPage stores discovered targets before acknowledging cursor advancement.
type InventoryPage struct {
	Sweep   SweepCursor `json:"sweep"`
	Targets []Target    `json:"targets"`
}

// Inventory performs one bounded page read or target registration batch.
func (coordinator *Coordinator) Inventory(ctx context.Context, cursor SweepCursor) InventoryPage {
	now := coordinator.Clock()
	if cursor.Stage == inventoryInitial {
		version := cursor.Version
		cursor = SweepCursor{Version: version, Stage: inventoryInstallations, InstallationPage: 1, InstallationIndex: 0, InstallationIDs: nil, InstallationsNext: false, RepositoryPage: 1, RepositoryIndex: 0, Repositories: nil, RepositoriesNext: false, PullRequestPage: 1, PullRequestIndex: 0, PullRequests: nil, PullRequestsNext: false, NotBeforeMS: now.UnixMilli()}
	}
	var result InventoryPage
	result.Sweep = cursor
	result.Sweep.NotBeforeMS = now.Add(time.Second).UnixMilli()
	if !coordinator.Settings.Enabled {
		result.Sweep.NotBeforeMS = coordinator.nextReconciliation(now).UnixMilli()
		return result
	}
	var err error
	switch cursor.Stage {
	case inventoryInitial:
		err = errors.New("inventory initialization did not select its first page")
	case inventoryInstallations:
		err = coordinator.inventoryInstallations(ctx, &result)
	case inventoryRepositories:
		err = coordinator.inventoryRepositories(ctx, &result)
	case inventoryPullRequests:
		err = coordinator.inventoryPullRequests(ctx, &result)
	case inventoryTargets:
		if cursor.PullRequestIndex < 0 || cursor.PullRequestIndex > len(cursor.PullRequests) {
			err = errors.New("inventory target cursor is invalid")
		} else {
			coordinator.inventoryTargets(&result, now)
		}
	default:
		err = errors.New("inventory cursor stage is invalid")
	}
	if err != nil {
		coordinator.Logger.WarnContext(ctx, "reassessment inventory deferred", slog.String("err", err.Error()))
		result.Sweep = cursor
		result.Sweep.NotBeforeMS = coordinator.delayed(now, 0).UnixMilli()
		result.Targets = nil
	}
	if result.Sweep.Stage == inventoryInitial {
		result.Sweep.NotBeforeMS = coordinator.nextReconciliation(now).UnixMilli()
	}
	return result
}

func (coordinator *Coordinator) inventoryInstallations(ctx context.Context, result *InventoryPage) error {
	cursor := &result.Sweep
	installations, next, err := coordinator.Discovery.ListInstallationsPage(ctx, cursor.InstallationPage, coordinator.Settings.PageSize)
	if err != nil {
		coordinator.Logger.WarnContext(ctx, "read inventory installations", slog.String("err", err.Error()))
		return fmt.Errorf("read inventory installations: %w", err)
	}
	cursor.InstallationIDs = nil
	for _, installation := range installations {
		cursor.InstallationIDs = append(cursor.InstallationIDs, installation.ID)
	}
	cursor.InstallationIndex = 0
	cursor.InstallationsNext = next
	if len(installations) == 0 {
		if next {
			cursor.InstallationPage++
			return nil
		}
		cursor.Stage = inventoryInitial
		return nil
	}
	cursor.RepositoryPage = 1
	cursor.Stage = inventoryRepositories
	return nil
}

func (coordinator *Coordinator) inventoryRepositories(ctx context.Context, result *InventoryPage) error {
	cursor := &result.Sweep
	if cursor.InstallationIndex < 0 || cursor.InstallationIndex >= len(cursor.InstallationIDs) {
		return errors.New("inventory installation cursor is invalid")
	}
	repositories, next, err := coordinator.Discovery.ListInstallationRepositoriesPage(ctx, cursor.InstallationIDs[cursor.InstallationIndex], cursor.RepositoryPage, coordinator.Settings.PageSize)
	if err != nil {
		coordinator.Logger.WarnContext(ctx, "read inventory repositories", slog.String("err", err.Error()))
		return fmt.Errorf("read inventory repositories: %w", err)
	}
	cursor.Repositories = repositories
	cursor.RepositoryIndex = 0
	cursor.RepositoriesNext = next
	if len(repositories) == 0 {
		if next {
			cursor.RepositoryPage++
			return nil
		}
		advanceInstallation(cursor)
		return nil
	}
	cursor.PullRequestPage = 1
	cursor.Stage = inventoryPullRequests
	return nil
}

func (coordinator *Coordinator) inventoryPullRequests(ctx context.Context, result *InventoryPage) error {
	cursor := &result.Sweep
	if cursor.InstallationIndex < 0 || cursor.InstallationIndex >= len(cursor.InstallationIDs) || cursor.RepositoryIndex < 0 || cursor.RepositoryIndex >= len(cursor.Repositories) {
		return errors.New("inventory repository cursor is invalid")
	}
	installationID := cursor.InstallationIDs[cursor.InstallationIndex]
	repository := cursor.Repositories[cursor.RepositoryIndex]
	requests, next, err := coordinator.Discovery.ListOpenPullRequestsPage(ctx, installationID, repository, cursor.PullRequestPage, coordinator.Settings.PageSize)
	if err != nil {
		coordinator.Logger.WarnContext(ctx, "read inventory pull requests", slog.String("err", err.Error()))
		return fmt.Errorf("read inventory pull requests: %w", err)
	}
	cursor.PullRequests = nil
	for _, request := range requests {
		cursor.PullRequests = append(cursor.PullRequests, domain.PullRequestRef{Repository: repository, InstallationID: installationID, Number: request.Number, Head: request.Head})
	}
	cursor.PullRequestIndex = 0
	cursor.PullRequestsNext = next
	if len(requests) == 0 {
		if next {
			cursor.PullRequestPage++
			return nil
		}
		advanceRepository(cursor)
		return nil
	}
	cursor.Stage = inventoryTargets
	return nil
}

func (coordinator *Coordinator) inventoryTargets(result *InventoryPage, now time.Time) {
	cursor := &result.Sweep
	end := min(len(cursor.PullRequests), cursor.PullRequestIndex+coordinator.Settings.BatchSize)
	for cursor.PullRequestIndex < end {
		var job domain.ReviewJob
		job.PullRequestRef = cursor.PullRequests[cursor.PullRequestIndex]
		target := coordinator.NewTarget(job, now)
		target.NotBeforeMS = now.UnixMilli()
		result.Targets = append(result.Targets, target)
		cursor.PullRequestIndex++
	}
	if cursor.PullRequestIndex >= len(cursor.PullRequests) {
		cursor.PullRequests = nil
		if cursor.PullRequestsNext {
			cursor.PullRequestPage++
			cursor.Stage = inventoryPullRequests
		} else {
			advanceRepository(cursor)
		}
	}
}

func advanceRepository(cursor *SweepCursor) {
	cursor.RepositoryIndex++
	cursor.PullRequestPage = 1
	if cursor.RepositoryIndex < len(cursor.Repositories) {
		cursor.Stage = inventoryPullRequests
		return
	}
	if cursor.RepositoriesNext {
		cursor.RepositoryPage++
		cursor.Stage = inventoryRepositories
		return
	}
	advanceInstallation(cursor)
}

func advanceInstallation(cursor *SweepCursor) {
	cursor.InstallationIndex++
	cursor.RepositoryPage = 1
	if cursor.InstallationIndex < len(cursor.InstallationIDs) {
		cursor.Stage = inventoryRepositories
		return
	}
	if cursor.InstallationsNext {
		cursor.InstallationPage++
		cursor.Stage = inventoryInstallations
		return
	}
	cursor.Stage = inventoryInitial
}

func (coordinator *Coordinator) nextReconciliation(now time.Time) time.Time {
	duration, _ := time.ParseDuration(coordinator.Settings.ReconcileInterval)
	return now.Add(duration)
}

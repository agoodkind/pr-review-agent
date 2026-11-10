package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

type pullRequestResponse struct {
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Number int    `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
	Draft   bool   `json:"draft"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Commits int    `json:"commits"`
}

// Comparison contains local changes and their verified merge base.
type Comparison struct {
	MergeBase domain.HeadSHA
	Files     []ChangedFile
}

// GetPullRequest loads one pull request by number.
func (client *Client) GetPullRequest(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
) (PullRequest, error) {
	path := client.repoPath(repo, fmt.Sprintf("/pulls/%d", number))
	body, err := client.doREST(ctx, installationID, "GET", path, nil)
	if err != nil {
		return PullRequest{}, err
	}

	var response pullRequestResponse
	if err := json.Unmarshal(body, &response); err != nil {
		client.logger.ErrorContext(ctx, "decode pull request", slog.String("err", err.Error()))
		return PullRequest{}, errors.New("decode pull request")
	}

	head, err := parseHeadSHA(response.Head.SHA)
	if err != nil {
		return PullRequest{}, err
	}

	base, err := parseHeadSHA(response.Base.SHA)
	if err != nil {
		return PullRequest{}, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		client.logger.WarnContext(
			ctx,
			"read pull request field presence",
			slog.String("err", err.Error()),
		)

		return PullRequest{}, fmt.Errorf("read pull request field presence: %w", err)
	}

	eligibilityKnown := response.Number == number &&
		(response.State == "open" || response.State == "closed") &&
		(response.State != "open" || !response.Merged) &&
		len(fields["draft"]) > 0 &&
		len(fields["merged"]) > 0 &&
		!bytes.Equal(bytes.TrimSpace(fields["draft"]), []byte("null")) &&
		!bytes.Equal(bytes.TrimSpace(fields["merged"]), []byte("null"))
	metadataKnown := strings.TrimSpace(response.Title) != "" && len(fields["body"]) > 0
	pullRequest := PullRequest{
		EligibilityKnown: eligibilityKnown,
		MetadataKnown:    metadataKnown,
		State:            response.State,
		Merged:           response.Merged,
		Number:           response.Number,
		Head:             head,
		Base:             base,
		Draft:            response.Draft,
		Title:            response.Title,
		Body:             response.Body,
		CommitCount:      response.Commits,
	}

	scope, err := client.repositorySnapshotScope(ctx, installationID, repo)
	if err != nil {
		return PullRequest{}, err
	}

	if scope != nil && head == scope.head {
		err := scope.lock(ctx)
		if err != nil {
			return PullRequest{}, err
		}

		if scope.pullRequest == nil {
			scope.pullRequest = &pullRequest
		}

		scope.unlock()
	}

	return pullRequest, nil
}

// ListPullRequestCommits follows pagination without truncating commit messages.
func (client *Client) ListPullRequestCommits(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
) ([]PullRequestCommit, error) {
	var commits []PullRequestCommit
	path := client.repoPath(repo, fmt.Sprintf("/pulls/%d/commits?per_page=100", number))
	err := client.doRESTPaginated(
		ctx,
		installationID,
		path,
		func(body []byte) (int, error) {
			var page []struct {
				SHA    string `json:"sha"`
				Commit struct {
					Message string `json:"message"`
				} `json:"commit"`
			}
			if err := json.Unmarshal(body, &page); err != nil {
				client.logger.WarnContext(ctx, "Decode pull request commits failed", "err", err)
				return 0, fmt.Errorf("decode pull request commits: %w", err)
			}

			for _, item := range page {
				sha, err := parseHeadSHA(item.SHA)
				if err != nil {
					return 0, err
				}

				commits = append(commits, PullRequestCommit{SHA: sha, Message: item.Commit.Message})
			}

			return len(page), nil
		},
	)

	return commits, err
}

// ListChangedFiles computes every changed path from the pinned Git revisions.
//
// An active review reuses its initial pull request metadata
// and cached Git objects.
//
// Example:
//
//	files, err := client.ListChangedFiles(ctx, installationID, repo, number)
func (client *Client) ListChangedFiles(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
) ([]ChangedFile, error) {
	scope, err := client.repositorySnapshotScope(ctx, installationID, repo)
	if err != nil {
		return nil, err
	}

	var pullRequest PullRequest

	if scope != nil {
		err := scope.lock(ctx)
		if err != nil {
			return nil, err
		}

		if scope.pullRequest != nil && scope.pullRequest.Number == number {
			pullRequest = *scope.pullRequest
		}

		scope.unlock()
	}

	if pullRequest.Head == "" {
		pullRequest, err = client.GetPullRequest(ctx, installationID, repo, number)
		if err != nil {
			return nil, err
		}
	}

	comparison, err := client.Compare(ctx, installationID, repo, pullRequest.Base, pullRequest.Head)

	return comparison.Files, err
}

// FileContent separates file bytes from repository metadata outside the file.
type FileContent struct {
	Data     []byte
	Metadata string
}

// GetFile loads pinned Git file bytes or a submodule pointer and URL.
//
// GetFile prefixes repository metadata when the Git entry requires it.
//
// Example:
//
//	content, err := client.GetFile(ctx, installationID, repo, path, ref)
func (client *Client) GetFile(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	path string,
	ref domain.HeadSHA,
) ([]byte, error) {
	content, err := client.GetFileContent(ctx, installationID, repo, path, ref)
	if err != nil {
		return nil, err
	}

	if content.Metadata == "" {
		return content.Data, nil
	}

	return append([]byte(content.Metadata+"\n"), content.Data...), nil
}

// GetFileContent reads pinned Git bytes and separate repository metadata.
//
// A temporary scope deletes extracted files after a standalone read.
// An active review retains its extracted files until the review scope closes.
//
// Example:
//
//	content, err := client.GetFileContent(ctx, installationID, repo, path, ref)
func (client *Client) GetFileContent(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	path string,
	ref domain.HeadSHA,
) (FileContent, error) {
	pathError := validateSnapshotPath(path)
	if pathError != nil {
		return FileContent{}, pathError
	}

	scope, err := client.repositorySnapshotScope(ctx, installationID, repo)
	if err != nil {
		return FileContent{}, err
	}

	if scope == nil {
		var closeSnapshot func() error

		ctx, closeSnapshot, err = client.OpenRepositorySnapshot(ctx, installationID, repo, ref)
		if err != nil {
			return FileContent{}, err
		}

		defer func() { _ = closeSnapshot() }()

		scope, err = client.repositorySnapshotScope(ctx, installationID, repo)
		if err != nil {
			return FileContent{}, err
		}
	}

	return scope.read(ctx, path, ref)
}

// Compare computes local patches from the verified merge base
// to the exact head.
//
// Compare requests more ancestry only when cached commits cannot establish
// the merge base.
//
// Example:
//
//	comparison, err := client.Compare(ctx, installationID, repo, base, head)
func (client *Client) Compare(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	base domain.HeadSHA,
	head domain.HeadSHA,
) (Comparison, error) {
	_, scopeError := client.repositorySnapshotScope(ctx, installationID, repo)
	if scopeError != nil {
		return Comparison{}, scopeError
	}

	return client.compareRepositoryCommits(ctx, installationID, repo, base, head, "")
}

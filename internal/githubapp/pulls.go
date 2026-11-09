package githubapp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
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

type changedFileResponse struct {
	Filename         string  `json:"filename"`
	PreviousFilename string  `json:"previous_filename"`
	Status           string  `json:"status"`
	Patch            *string `json:"patch"`
	Additions        *int    `json:"additions"`
	Deletions        *int    `json:"deletions"`
}

type compareResponse struct {
	MergeBaseCommit struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
	Files []changedFileResponse `json:"files"`
}

// Comparison is what comparing two commits returned: the files that changed, and
// the commit their patches are measured from.
//
// The merge base is part of the answer because GitHub does not diff the base
// against the head directly. When the two have diverged it diffs from where they
// last agreed, so the old side of every patch belongs to the merge base rather
// than to the base that was asked for. A caller mapping coordinates through those
// patches would be mapping from a commit it never named, and landing on
// unrelated code. MergeBase is empty when the response did not carry a usable
// one, which callers must read as "do not assume".
type Comparison struct {
	MergeBase domain.HeadSHA
	Files     []ChangedFile
}

type fileContentResponse struct {
	Content         string `json:"content"`
	Encoding        string `json:"encoding"`
	Type            string `json:"type"`
	SHA             string `json:"sha"`
	SubmoduleGitURL string `json:"submodule_git_url"`
	Size            *int   `json:"size"`
}

// GetPullRequest loads one pull request by number.
//
// The client requests the repository pull request endpoint and decodes its
// response.
//
// For example:
//
//	pullRequest, err := client.GetPullRequest(ctx, installationID, repo, 12)
func (client *Client) GetPullRequest(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
) (PullRequest, error) {
	path := client.repoPath(repo, fmt.Sprintf("/pulls/%d", number))
	body, err := client.doREST(ctx, installationID, "GET", path, nil, nil)
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

	return PullRequest{
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
	}, nil
}

// ListPullRequestCommits reads every commit on a pull request.
//
// The client follows pagination and preserves complete commit messages.
//
// For example:
//
//	commits, err := client.ListPullRequestCommits(
//		ctx,
//		installationID,
//		repo,
//		12,
//	)
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

// ListChangedFiles reads every changed file on a pull request.
//
// The client follows pagination before returning the combined file list.
//
// For example:
//
//	files, err := client.ListChangedFiles(ctx, installationID, repo, 12)
func (client *Client) ListChangedFiles(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
) ([]ChangedFile, error) {
	path := client.repoPath(repo, fmt.Sprintf("/pulls/%d/files", number))
	files := make([]ChangedFile, 0)
	err := client.doRESTPaginated(
		ctx,
		installationID,
		path,
		func(page []byte) (int, error) {
			var pageFiles []changedFileResponse
			if err := json.Unmarshal(page, &pageFiles); err != nil {
				client.logger.ErrorContext(
					ctx,
					"decode changed files page",
					slog.String("err", err.Error()),
				)
				return 0, errors.New("decode changed files page")
			}

			for _, item := range pageFiles {
				file, err := decodeChangedFile(item)
				if err != nil {
					return 0, err
				}

				files = append(files, file)
			}

			return len(pageFiles), nil
		},
	)
	if err != nil {
		return nil, err
	}

	return files, nil
}

// FileContent separates file bytes from repository metadata outside the file.
type FileContent struct {
	Data     []byte
	Metadata string
}

// GetFile reads file bytes or submodule metadata at one commit.
//
// The client returns submodule metadata as bytes when the requested path
// identifies a submodule.
//
// For example:
//
//	data, err := client.GetFile(
//		ctx,
//		installationID,
//		repo,
//		"README.md",
//		head,
//	)
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

// GetFileContent reads file bytes and submodule metadata separately.
//
// An active repository snapshot supplies the requested immutable file
// content.
//
// For example:
//
//	content, err := client.GetFileContent(
//		ctx,
//		installationID,
//		repo,
//		path,
//		head,
//	)
func (client *Client) GetFileContent(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	path string,
	ref domain.HeadSHA,
) (FileContent, error) {
	scope, err := client.repositorySnapshotScope(ctx, installationID, repo)
	if err != nil {
		return FileContent{}, err
	}

	if scope != nil {
		return scope.read(ctx, path, ref)
	}

	escapedPath := strings.Split(path, "/")
	for index, segment := range escapedPath {
		escapedPath[index] = url.PathEscape(segment)
	}

	apiPath := client.repoPath(repo, "/contents/"+strings.Join(escapedPath, "/"))
	query := url.Values{}
	query.Set("ref", string(ref))

	body, err := client.doREST(ctx, installationID, "GET", apiPath, query, nil)
	if err != nil {
		return FileContent{}, err
	}

	var response fileContentResponse
	if err := json.Unmarshal(body, &response); err != nil {
		client.logger.ErrorContext(ctx, "decode file content", slog.String("err", err.Error()))
		return FileContent{}, errors.New("decode file content")
	}

	if response.Type == "submodule" {
		return submoduleFileContent(response.SHA, response.SubmoduleGitURL)
	}

	if response.Encoding == "none" && response.Type == "file" {
		content, err := client.getFileBlob(ctx, installationID, repo, response)
		if err != nil {
			return FileContent{}, err
		}

		return FileContent{Data: content, Metadata: ""}, nil
	}

	if response.Encoding != "base64" {
		return FileContent{}, errors.New("unsupported file encoding")
	}

	normalized := strings.ReplaceAll(response.Content, "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(normalized)
	if err != nil {
		client.logger.ErrorContext(ctx, "decode file content", slog.String("err", err.Error()))
		return FileContent{}, errors.New("decode file content")
	}

	return FileContent{Data: decoded, Metadata: ""}, nil
}

func submoduleFileContent(revision, repositoryURL string) (FileContent, error) {
	commit, err := domain.ParseHeadSHA(revision)
	if err != nil || strings.TrimSpace(repositoryURL) == "" ||
		strings.ContainsAny(repositoryURL, "\r\n\x00") {
		return FileContent{}, errors.New("invalid submodule metadata")
	}

	return FileContent{
		Data:     []byte(fmt.Sprintf("Subproject commit %s\n", commit)),
		Metadata: "Submodule repository: " + repositoryURL,
	}, nil
}

// Compare reads the files changed between two commits.
//
// The result includes the merge base that GitHub uses for the returned
// patches.
//
// For example:
//
//	comparison, err := client.Compare(ctx, installationID, repo, base, head)
func (client *Client) Compare(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	base domain.HeadSHA,
	head domain.HeadSHA,
) (Comparison, error) {
	path := client.repoPath(repo, fmt.Sprintf("/compare/%s...%s", base, head))
	comparison := Comparison{MergeBase: "", Files: make([]ChangedFile, 0)}
	err := client.doRESTPaginated(
		ctx,
		installationID,
		path,
		func(page []byte) (int, error) {
			var response compareResponse
			if err := json.Unmarshal(page, &response); err != nil {
				client.logger.ErrorContext(
					ctx,
					"decode compare page",
					slog.String("err", err.Error()),
				)
				return 0, errors.New("decode compare page")
			}

			// Every page repeats the merge base; the first usable one settles it. A
			// SHA that does not parse leaves it empty, which reads as unknown rather
			// than as a commit nobody verified.
			if comparison.MergeBase == "" {
				if mergeBase, err := parseHeadSHA(response.MergeBaseCommit.SHA); err == nil {
					comparison.MergeBase = mergeBase
				}
			}

			for _, item := range response.Files {
				file, err := decodeChangedFile(item)
				if err != nil {
					return 0, err
				}

				comparison.Files = append(comparison.Files, file)
			}

			return len(response.Files), nil
		},
	)
	if err != nil {
		return Comparison{MergeBase: "", Files: nil}, err
	}

	return comparison, nil
}

func decodeChangedFile(item changedFileResponse) (ChangedFile, error) {
	if item.Additions != nil && *item.Additions < 0 {
		return ChangedFile{}, errors.New("changed file additions cannot be negative")
	}

	if item.Deletions != nil && *item.Deletions < 0 {
		return ChangedFile{}, errors.New("changed file deletions cannot be negative")
	}

	changed := ChangedFile{
		Path:              item.Filename,
		PreviousPath:      item.PreviousFilename,
		Status:            item.Status,
		Patch:             "",
		PatchPresent:      false,
		Additions:         0,
		Deletions:         0,
		ChangeCountsKnown: false,
	}
	if item.Additions != nil && item.Deletions != nil {
		changed.Additions = *item.Additions
		changed.Deletions = *item.Deletions
		changed.ChangeCountsKnown = true
	}

	if item.Patch != nil {
		changed.Patch = *item.Patch
		changed.PatchPresent = true
	}

	return changed, nil
}

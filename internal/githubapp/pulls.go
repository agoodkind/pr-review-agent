package githubapp

import (
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
	Number int `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
	Draft bool   `json:"draft"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

type changedFileResponse struct {
	Filename         string  `json:"filename"`
	PreviousFilename string  `json:"previous_filename"`
	Status           string  `json:"status"`
	Patch            *string `json:"patch"`
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
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// GetPullRequest loads one pull request by number.
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

	return PullRequest{
		Number: response.Number,
		Head:   head,
		Base:   base,
		Draft:  response.Draft,
		Title:  response.Title,
		Body:   response.Body,
	}, nil
}

// ListChangedFiles returns every file changed on one pull request.
func (client *Client) ListChangedFiles(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
) ([]ChangedFile, error) {
	path := client.repoPath(repo, fmt.Sprintf("/pulls/%d/files", number))
	files := make([]ChangedFile, 0)
	err := client.doRESTPaginated(ctx, installationID, path, func(page []byte) (int, error) {
		var pageFiles []changedFileResponse
		if err := json.Unmarshal(page, &pageFiles); err != nil {
			client.logger.ErrorContext(ctx, "decode changed files page", slog.String("err", err.Error()))
			return 0, errors.New("decode changed files page")
		}
		for _, item := range pageFiles {
			files = append(files, decodeChangedFile(item))
		}
		return len(pageFiles), nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// GetFile loads repository file bytes at one ref.
func (client *Client) GetFile(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	path string,
	ref domain.HeadSHA,
) ([]byte, error) {
	escapedPath := strings.Split(path, "/")
	for index, segment := range escapedPath {
		escapedPath[index] = url.PathEscape(segment)
	}
	apiPath := client.repoPath(repo, "/contents/"+strings.Join(escapedPath, "/"))
	query := url.Values{}
	query.Set("ref", string(ref))

	body, err := client.doREST(ctx, installationID, "GET", apiPath, query, nil)
	if err != nil {
		return nil, err
	}

	var response fileContentResponse
	if err := json.Unmarshal(body, &response); err != nil {
		client.logger.ErrorContext(ctx, "decode file content", slog.String("err", err.Error()))
		return nil, errors.New("decode file content")
	}
	if response.Encoding != "base64" {
		return nil, errors.New("unsupported file encoding")
	}

	normalized := strings.ReplaceAll(response.Content, "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(normalized)
	if err != nil {
		client.logger.ErrorContext(ctx, "decode file content", slog.String("err", err.Error()))
		return nil, errors.New("decode file content")
	}
	return decoded, nil
}

// Compare returns the files changed between two commits and the commit their
// patches are measured from.
func (client *Client) Compare(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	base domain.HeadSHA,
	head domain.HeadSHA,
) (Comparison, error) {
	path := client.repoPath(repo, fmt.Sprintf("/compare/%s...%s", base, head))
	comparison := Comparison{MergeBase: "", Files: make([]ChangedFile, 0)}
	err := client.doRESTPaginated(ctx, installationID, path, func(page []byte) (int, error) {
		var response compareResponse
		if err := json.Unmarshal(page, &response); err != nil {
			client.logger.ErrorContext(ctx, "decode compare page", slog.String("err", err.Error()))
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
			comparison.Files = append(comparison.Files, decodeChangedFile(item))
		}
		return len(response.Files), nil
	})
	if err != nil {
		return Comparison{MergeBase: "", Files: nil}, err
	}
	return comparison, nil
}

func decodeChangedFile(item changedFileResponse) ChangedFile {
	changed := ChangedFile{
		Path:         item.Filename,
		PreviousPath: item.PreviousFilename,
		Status:       item.Status,
		Patch:        "",
		PatchPresent: false,
	}
	if item.Patch != nil {
		changed.Patch = *item.Patch
		changed.PatchPresent = true
	}
	return changed
}

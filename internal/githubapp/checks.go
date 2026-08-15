package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"

	"goodkind.io/pr-review-agent/internal/domain"
)

type checkRunResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type checkRunsListResponse struct {
	CheckRuns []checkRunResponse `json:"check_runs"`
}

type createCheckRunBody struct {
	Name    string `json:"name"`
	HeadSHA string `json:"head_sha"`
	Status  string `json:"status"`
}

type checkRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

type updateCheckRunBody struct {
	Status     string          `json:"status,omitempty"`
	Conclusion string          `json:"conclusion,omitempty"`
	DetailsURL string          `json:"details_url,omitempty"`
	Output     *checkRunOutput `json:"output,omitempty"`
}

// FindCheckRun returns one check run by head SHA and name.
func (client *Client) FindCheckRun(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	head domain.HeadSHA,
	name string,
) (CheckRun, bool, error) {
	path := client.repoPath(repo, fmt.Sprintf("/commits/%s/check-runs", url.PathEscape(string(head))))
	query := url.Values{}
	query.Set("check_name", name)

	body, err := client.doREST(ctx, installationID, "GET", path, query, nil)
	if err != nil {
		return CheckRun{}, false, err
	}

	var response checkRunsListResponse
	if err := json.Unmarshal(body, &response); err != nil {
		client.logger.ErrorContext(ctx, "decode check runs", slog.String("err", err.Error()))
		return CheckRun{}, false, errors.New("decode check runs")
	}

	for _, item := range response.CheckRuns {
		if item.Name != name {
			continue
		}
		headSHA, err := parseHeadSHA(item.HeadSHA)
		if err != nil {
			return CheckRun{}, false, err
		}
		return CheckRun{
			ID:         item.ID,
			Name:       item.Name,
			Head:       headSHA,
			Status:     item.Status,
			Conclusion: item.Conclusion,
		}, true, nil
	}
	return CheckRun{
		ID:         0,
		Name:       "",
		Head:       "",
		Status:     "",
		Conclusion: "",
	}, false, nil
}

// CreateCheckRun creates one queued check run for a head commit.
func (client *Client) CreateCheckRun(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	head domain.HeadSHA,
	name string,
) (CheckRun, error) {
	path := client.repoPath(repo, "/check-runs")
	payload := createCheckRunBody{
		Name:    name,
		HeadSHA: string(head),
		Status:  "queued",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		client.logger.ErrorContext(ctx, "marshal create check run body", slog.String("err", err.Error()))
		return CheckRun{}, errors.New("marshal create check run body")
	}

	body, err := client.doREST(ctx, installationID, "POST", path, nil, encoded)
	if err != nil {
		return CheckRun{}, err
	}

	return decodeCheckRun(body, ctx, client)
}

// StartCheckRun marks one check run in progress.
func (client *Client) StartCheckRun(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	checkRunID int64,
	detailsURL string,
) error {
	path := client.repoPath(repo, fmt.Sprintf("/check-runs/%d", checkRunID))
	payload := updateCheckRunBody{
		Status:     "in_progress",
		Conclusion: "",
		DetailsURL: detailsURL,
		Output:     nil,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		client.logger.ErrorContext(ctx, "marshal start check run body", slog.String("err", err.Error()))
		return errors.New("marshal start check run body")
	}
	_, err = client.doREST(ctx, installationID, "PATCH", path, nil, encoded)
	return err
}

// CompleteCheckRun marks one check run completed with a conclusion.
func (client *Client) CompleteCheckRun(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	checkRunID int64,
	conclusion string,
	title string,
	summary string,
) error {
	path := client.repoPath(repo, fmt.Sprintf("/check-runs/%d", checkRunID))
	payload := updateCheckRunBody{
		Status:     "completed",
		Conclusion: conclusion,
		DetailsURL: "",
		Output: &checkRunOutput{
			Title:   title,
			Summary: summary,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		client.logger.ErrorContext(ctx, "marshal complete check run body", slog.String("err", err.Error()))
		return errors.New("marshal complete check run body")
	}
	_, err = client.doREST(ctx, installationID, "PATCH", path, nil, encoded)
	return err
}

func decodeCheckRun(body []byte, ctx context.Context, client *Client) (CheckRun, error) {
	var response checkRunResponse
	if err := json.Unmarshal(body, &response); err != nil {
		client.logger.ErrorContext(ctx, "decode check run", slog.String("err", err.Error()))
		return CheckRun{}, errors.New("decode check run")
	}
	head, err := parseHeadSHA(response.HeadSHA)
	if err != nil {
		return CheckRun{}, err
	}
	return CheckRun{
		ID:         response.ID,
		Name:       response.Name,
		Head:       head,
		Status:     response.Status,
		Conclusion: response.Conclusion,
	}, nil
}

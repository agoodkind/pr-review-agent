package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

// MaximumCheckRunTextBytes is the largest text body GitHub accepts on a check
// run output. A longer log is cut from the front, because the end of a run
// holds the failure and the front holds setup.
const MaximumCheckRunTextBytes = 65535

const checkRunTextTruncationNotice = "_Earlier lines omitted; this log was truncated to fit the check run._\n\n"

// TruncateCheckRunText keeps the tail of a log within the check run text limit.
func TruncateCheckRunText(text string) string {
	if len(text) <= MaximumCheckRunTextBytes {
		return text
	}
	budget := MaximumCheckRunTextBytes - len(checkRunTextTruncationNotice)
	tail := text[len(text)-budget:]
	if newline := strings.IndexByte(tail, '\n'); newline >= 0 && newline+1 < len(tail) {
		tail = tail[newline+1:]
	}
	return checkRunTextTruncationNotice + tail
}

type checkRunResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	ExternalID string `json:"external_id"`
}

type checkRunsListResponse struct {
	CheckRuns []checkRunResponse `json:"check_runs"`
}

type createCheckRunBody struct {
	Name    string `json:"name"`
	HeadSHA string `json:"head_sha"`
	Status  string `json:"status"`
	// ExternalID is GitHub's reference field for the caller's own identifier.
	// This service writes the webhook delivery that created the check run, so a
	// redelivery can recognize its own earlier work on GitHub rather than in
	// memory.
	ExternalID string `json:"external_id,omitempty"`
}

// checkRunOutput carries what GitHub renders inside the check run. Text holds
// the run's own log, so a failed review explains itself where the reader
// clicked instead of only naming the stage that failed.
type checkRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Text    string `json:"text,omitempty"`
}

type updateCheckRunBody struct {
	Status     string          `json:"status,omitempty"`
	Conclusion string          `json:"conclusion,omitempty"`
	DetailsURL string          `json:"details_url,omitempty"`
	Output     *checkRunOutput `json:"output,omitempty"`
}

// emptyCheckRun is the zero check run returned beside a miss or a failure.
func emptyCheckRun() CheckRun {
	return CheckRun{
		ID:         0,
		Name:       "",
		Head:       "",
		Status:     "",
		Conclusion: "",
		ExternalID: "",
	}
}

// listCheckRuns returns every check run of one name on one head, newest last as
// GitHub lists them.
func (client *Client) listCheckRuns(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	head domain.HeadSHA,
	name string,
) ([]CheckRun, error) {
	path := client.repoPath(repo, fmt.Sprintf("/commits/%s/check-runs", url.PathEscape(string(head))))
	query := url.Values{}
	query.Set("check_name", name)

	body, err := client.doREST(ctx, installationID, "GET", path, query, nil)
	if err != nil {
		return nil, err
	}

	var response checkRunsListResponse
	if err := json.Unmarshal(body, &response); err != nil {
		client.logger.ErrorContext(ctx, "decode check runs", slog.String("err", err.Error()))
		return nil, errors.New("decode check runs")
	}

	runs := make([]CheckRun, 0, len(response.CheckRuns))
	for _, item := range response.CheckRuns {
		if item.Name != name {
			continue
		}
		headSHA, err := parseHeadSHA(item.HeadSHA)
		if err != nil {
			return nil, err
		}
		runs = append(runs, CheckRun{
			ID:         item.ID,
			Name:       item.Name,
			Head:       headSHA,
			Status:     item.Status,
			Conclusion: item.Conclusion,
			ExternalID: item.ExternalID,
		})
	}
	return runs, nil
}

// FindCheckRun returns one check run by head SHA and name.
func (client *Client) FindCheckRun(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	head domain.HeadSHA,
	name string,
) (CheckRun, bool, error) {
	runs, err := client.listCheckRuns(ctx, installationID, repo, head, name)
	if err != nil {
		return emptyCheckRun(), false, err
	}
	if len(runs) == 0 {
		return emptyCheckRun(), false, nil
	}
	return runs[0], true, nil
}

// FindCheckRunByExternalID returns the check run one webhook delivery already
// created on this head, which is how a redelivery recognizes its own earlier
// work.
//
// It reads GitHub rather than any in process record on purpose. The delivery
// cache lives in the container, and the delivery this matters for is the one
// that restarts the container, so by the time a redelivery arrives there is no
// in process record of the first one left to consult.
func (client *Client) FindCheckRunByExternalID(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	head domain.HeadSHA,
	name string,
	externalID string,
) (CheckRun, bool, error) {
	if externalID == "" {
		return emptyCheckRun(), false, nil
	}
	runs, err := client.listCheckRuns(ctx, installationID, repo, head, name)
	if err != nil {
		return emptyCheckRun(), false, err
	}
	for _, run := range runs {
		if run.ExternalID == externalID {
			return run, true, nil
		}
	}
	return emptyCheckRun(), false, nil
}

// CreateCheckRun creates one queued check run for a head commit, stamped with
// the delivery that created it.
func (client *Client) CreateCheckRun(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	head domain.HeadSHA,
	name string,
	externalID string,
) (CheckRun, error) {
	path := client.repoPath(repo, "/check-runs")
	payload := createCheckRunBody{
		Name:       name,
		HeadSHA:    string(head),
		Status:     "queued",
		ExternalID: externalID,
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

// CompleteCheckRun marks one check run completed with a conclusion. The text
// argument carries the run's own log and is truncated to what GitHub accepts.
func (client *Client) CompleteCheckRun(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	checkRunID int64,
	conclusion string,
	title string,
	summary string,
	text string,
) error {
	path := client.repoPath(repo, fmt.Sprintf("/check-runs/%d", checkRunID))
	payload := updateCheckRunBody{
		Status:     "completed",
		Conclusion: conclusion,
		DetailsURL: "",
		Output: &checkRunOutput{
			Title:   title,
			Summary: summary,
			Text:    TruncateCheckRunText(text),
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
		ExternalID: response.ExternalID,
	}, nil
}

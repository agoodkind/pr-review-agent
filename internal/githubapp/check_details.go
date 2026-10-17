package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"goodkind.io/pr-review-agent/internal/domain"
)

// CheckRunDetails includes the source and owner of one check summary.
type CheckRunDetails struct {
	Run     CheckRun
	AppID   int64
	Summary string
}

// GetCheckRunDetails reads one exact check run without selecting a newer run.
func (client *Client) GetCheckRunDetails(ctx context.Context, installationID int64, repo domain.Repository, checkRunID int64) (CheckRunDetails, error) {
	var details CheckRunDetails
	path := client.repoPath(repo, fmt.Sprintf("/check-runs/%d", checkRunID))
	body, err := client.doREST(ctx, installationID, http.MethodGet, path, nil, nil)
	if err != nil {
		return details, err
	}
	var response checkRunResponse
	if err := json.Unmarshal(body, &response); err != nil {
		client.logger.ErrorContext(ctx, "Decode check run details failed", slog.String("err", err.Error()))
		return details, errors.New("decode check run details")
	}
	details.Run, err = decodeCheckRun(body, ctx, client)
	if err != nil {
		return details, err
	}
	details.AppID = response.App.ID
	details.Summary = response.Output.Summary
	return details, nil
}

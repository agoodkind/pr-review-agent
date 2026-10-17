package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"goodkind.io/pr-review-agent/internal/domain"
)

// CheckRunDetails includes the source and owner of one check summary.
type CheckRunDetails struct {
	Run     CheckRun
	AppID   int64
	Summary string
}

type checkRunDetailsResponse checkRunResponse

func (response *checkRunDetailsResponse) UnmarshalJSON(data []byte) error {
	var decoded checkRunResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode check run details: %w", err)
	}
	*response = checkRunDetailsResponse(decoded)
	return nil
}

// GetCheckRunDetails reads one exact check run without selecting a newer run.
func (client *Client) GetCheckRunDetails(ctx context.Context, installationID int64, repo domain.Repository, checkRunID int64) (CheckRunDetails, error) {
	var details CheckRunDetails
	path := client.repoPath(repo, fmt.Sprintf("/check-runs/%d", checkRunID))
	body, err := client.doREST(ctx, installationID, http.MethodGet, path, nil, nil)
	if err != nil {
		return details, err
	}
	var response checkRunDetailsResponse
	if err := response.UnmarshalJSON(body); err != nil {
		return details, err
	}
	details.Run, err = decodeCheckRun(body, ctx, client)
	if err != nil {
		return details, err
	}
	details.AppID = response.App.ID
	details.Summary = response.Output.Summary
	return details, nil
}

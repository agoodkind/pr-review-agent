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

type checkRunDetailsResponse CheckRunDetails

func (details *checkRunDetailsResponse) UnmarshalJSON(data []byte) error {
	var decoded checkRunResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode check run details: %w", err)
	}
	head, err := parseHeadSHA(decoded.HeadSHA)
	if err != nil {
		return err
	}
	*details = checkRunDetailsResponse{
		Run: CheckRun{
			ID:         decoded.ID,
			Outcome:    domain.DecodeAssessmentOutcome(decoded.Output.Text),
			Name:       decoded.Name,
			Head:       head,
			Status:     decoded.Status,
			Conclusion: decoded.Conclusion,
			ExternalID: decoded.ExternalID,
		},
		AppID:   decoded.App.ID,
		Summary: decoded.Output.Summary,
	}
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
	return CheckRunDetails(response), nil
}

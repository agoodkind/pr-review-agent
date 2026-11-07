package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

const inspectionSearchCandidates = 30

// SearchRepository discovers indexed candidate paths without asserting current content.
func (client *Client) SearchRepository(ctx context.Context, installationID int64, repo domain.Repository, query string) ([]string, error) {
	if strings.TrimSpace(query) == "" || strings.ContainsAny(query, "\r\n\x00") {
		return nil, errors.New("repository discovery requires a nonempty literal query")
	}
	parameters := url.Values{}
	parameters.Set("q", strconv.Quote(query)+" repo:"+repo.Owner+"/"+repo.Name+" in:file")
	parameters.Set("per_page", strconv.Itoa(inspectionSearchCandidates))
	body, err := client.doREST(ctx, installationID, http.MethodGet, "/search/code", parameters, nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []struct {
			Path       string `json:"path"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, errors.New("decode repository discovery results")
	}
	var paths []string
	for _, item := range result.Items {
		if !strings.EqualFold(item.Repository.FullName, repo.Owner+"/"+repo.Name) || item.Path == "" {
			return nil, errors.New("repository discovery returned an unexpected repository or path")
		}
		if !slices.Contains(paths, item.Path) {
			paths = append(paths, item.Path)
		}
		if len(paths) >= inspectionSearchCandidates {
			break
		}
	}
	return paths, nil
}

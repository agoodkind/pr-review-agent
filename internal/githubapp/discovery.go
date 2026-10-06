package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

const maximumDiscoveryPageSize = 100

// Installation IDs select credentials for repository requests.
type Installation struct {
	ID int64 `json:"id"`
}

type discoveryRepository struct {
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
}

type discoveryPullRequest struct {
	pullRequestResponse
	Draft *bool `json:"draft"`
}

// ListInstallationsPage uses app authentication and reports GitHub's next-page link.
func (client *Client) ListInstallationsPage(ctx context.Context, page, pageSize int) ([]Installation, bool, error) {
	credential, err := buildAppJWT(client.cfg.GitHubPrivateKey, client.cfg.GitHubAppID, client.now())
	if err != nil {
		client.logger.ErrorContext(ctx, "GitHub discovery app authentication failed", slog.String("err", "app authentication could not be built"))
		return nil, false, errors.New("build discovery app authentication")
	}
	body, next, err := client.discoveryPage(ctx, "/app/installations", page, pageSize, credential, nil)
	if err != nil {
		return nil, false, err
	}
	var installations []Installation
	if json.Unmarshal(body, &installations) != nil || installations == nil {
		client.logger.ErrorContext(ctx, "GitHub installation page decoding failed", slog.String("err", "missing or invalid installation array"))
		return nil, false, errors.New("GitHub installation page omitted a valid array")
	}
	for _, installation := range installations {
		if installation.ID <= 0 {
			return nil, false, errors.New("GitHub installation page contains an invalid installation identifier")
		}
	}
	return installations, next, nil
}

// ListInstallationRepositoriesPage reports accessible repositories without an owner filter.
func (client *Client) ListInstallationRepositoriesPage(ctx context.Context, installationID int64, page, pageSize int) ([]domain.Repository, bool, error) {
	credential, err := client.installationToken(ctx, installationID)
	if err != nil {
		return nil, false, err
	}
	body, next, err := client.discoveryPage(ctx, "/installation/repositories", page, pageSize, credential, nil)
	if err != nil {
		return nil, false, err
	}
	var response struct {
		TotalCount   *int                  `json:"total_count"`
		Repositories []discoveryRepository `json:"repositories"`
	}
	if json.Unmarshal(body, &response) != nil || response.TotalCount == nil || response.Repositories == nil {
		client.logger.ErrorContext(ctx, "GitHub repository page decoding failed", slog.String("err", "missing or invalid repository count or array"))
		return nil, false, errors.New("GitHub repository page omitted count or repositories")
	}
	if *response.TotalCount < len(response.Repositories) {
		return nil, false, errors.New("GitHub repository page count is inconsistent")
	}
	repositories := make([]domain.Repository, 0, len(response.Repositories))
	for _, repository := range response.Repositories {
		if repository.Name == "" || repository.Owner.Login == "" {
			return nil, false, errors.New("GitHub repository page contains an incomplete repository identifier")
		}
		repositories = append(repositories, domain.Repository{Owner: repository.Owner.Login, Name: repository.Name})
	}
	return repositories, next, nil
}

// ListOpenPullRequestsPage includes drafts; GetPullRequest supplies complete review metadata.
func (client *Client) ListOpenPullRequestsPage(ctx context.Context, installationID int64, repository domain.Repository, page, pageSize int) ([]PullRequest, bool, error) {
	credential, err := client.installationToken(ctx, installationID)
	if err != nil {
		return nil, false, err
	}
	query := url.Values{"state": {"open"}}
	body, next, err := client.discoveryPage(ctx, client.repoPath(repository, "/pulls"), page, pageSize, credential, query)
	if err != nil {
		return nil, false, err
	}
	var response []discoveryPullRequest
	if json.Unmarshal(body, &response) != nil || response == nil {
		client.logger.ErrorContext(ctx, "GitHub open pull request page decoding failed", slog.String("err", "missing or invalid pull request array"))
		return nil, false, errors.New("GitHub open pull request page omitted a valid array")
	}
	pullRequests := make([]PullRequest, 0, len(response))
	for _, item := range response {
		if item.Number <= 0 || item.State != "open" || item.Draft == nil || item.Merged {
			return nil, false, errors.New("GitHub open pull request page contains invalid pull request metadata")
		}
		head, headErr := parseHeadSHA(item.Head.SHA)
		if headErr != nil {
			return nil, false, headErr
		}
		base, baseErr := parseHeadSHA(item.Base.SHA)
		if baseErr != nil {
			return nil, false, baseErr
		}
		pullRequests = append(pullRequests, PullRequest{
			EligibilityKnown: false,
			MetadataKnown:    false,
			State:            item.State,
			Merged:           item.Merged,
			Number:           item.Number,
			Head:             head,
			Base:             base,
			Draft:            *item.Draft,
			Title:            item.Title,
			Body:             item.Body,
			CommitCount:      item.Commits,
		})
	}
	return pullRequests, next, nil
}

func (client *Client) discoveryPage(ctx context.Context, path string, page, pageSize int, credential string, query url.Values) ([]byte, bool, error) {
	if page <= 0 || pageSize <= 0 || pageSize > maximumDiscoveryPageSize {
		return nil, false, errors.New("GitHub discovery requires a positive page and page size from 1 through 100")
	}
	if query == nil {
		query = make(url.Values)
	}
	query.Set("page", strconv.Itoa(page))
	query.Set("per_page", strconv.Itoa(pageSize))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.restURL(path)+"?"+query.Encode(), http.NoBody)
	if err != nil {
		client.logger.ErrorContext(ctx, "GitHub discovery request creation failed", slog.String("err", "request could not be created"))
		return nil, false, errors.New("create GitHub discovery request")
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", githubAcceptHeader)
	request.Header.Set(githubAPIVersionHeader, githubAPIVersionValue)
	response, err := client.httpClient.Do(request)
	if err != nil {
		client.logger.ErrorContext(ctx, "GitHub discovery request failed", slog.String("err", "HTTP request could not be completed"))
		return nil, false, fmt.Errorf("GitHub discovery request failed: %w", err)
	}
	body, readErr := readLimitedBody(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		client.logger.ErrorContext(ctx, "GitHub discovery response read failed", slog.String("err", "response could not be read within the size limit"))
		return nil, false, errors.New("read GitHub discovery response")
	}
	if closeErr != nil {
		client.logger.ErrorContext(ctx, "GitHub discovery response close failed", slog.String("err", "response body could not be closed"))
		return nil, false, errors.New("close GitHub discovery response")
	}
	if response.StatusCode != http.StatusOK {
		client.logger.ErrorContext(ctx, "GitHub discovery returned an unsuccessful status", slog.Int("api_status", response.StatusCode), slog.String("err", "HTTP status is not 200"))
		return nil, false, newAPIError(response.StatusCode, "GitHub discovery request failed")
	}
	next, err := discoveryHasNextPage(response.Header.Get("Link"), request.URL, page)
	if err != nil {
		client.logger.ErrorContext(ctx, "GitHub discovery pagination metadata is invalid", slog.String("err", "next-page link is invalid or does not advance"))
		return nil, false, err
	}
	return body, next, nil
}

func discoveryHasNextPage(header string, requested *url.URL, page int) (bool, error) {
	next, exists := parseNextLink(header)
	if !exists {
		if strings.Contains(header, `rel="next"`) {
			return false, errors.New("GitHub discovery next-page link is malformed")
		}
		return false, nil
	}
	parsed, err := url.Parse(next)
	if err != nil {
		return false, errors.New("GitHub discovery next-page URL is invalid")
	}
	resolved := requested.ResolveReference(parsed)
	if resolved.Scheme != requested.Scheme || resolved.Host != requested.Host || resolved.Path != requested.Path {
		return false, errors.New("GitHub discovery next-page link identifies a different endpoint")
	}
	nextPage, err := strconv.Atoi(resolved.Query().Get("page"))
	if err != nil || nextPage != page+1 {
		return false, errors.New("GitHub discovery pagination did not advance by one page")
	}
	return true, nil
}

// Package githubapp implements a GitHub App REST and GraphQL client.
package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
)

const (
	githubAcceptHeader     = "application/vnd.github+json"
	githubAPIVersionHeader = "X-Github-Api-Version"
	githubAPIVersionValue  = config.GitHubAPIVersion
	maxResponseBytes       = 16 * 1024 * 1024
)

// PullRequest is the pull request metadata required for review work.
type PullRequest struct {
	Number int
	Head   domain.HeadSHA
	Base   domain.HeadSHA
	Draft  bool
	Title  string
	Body   string
}

// ChangedFile is one file changed on a pull request or compare result.
type ChangedFile struct {
	Path         string
	PreviousPath string
	Status       string
	Patch        string
	PatchPresent bool
}

// Review is one submitted pull request review.
type Review struct {
	ID       int64
	CommitID domain.HeadSHA
	Author   string
	Body     string
	State    string
}

// InlineComment is one inline review comment submitted with a review.
type InlineComment struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Line      int    `json:"line"`
	Side      string `json:"side"`
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
}

// SubmitReviewRequest is the payload for one pull request review submission.
type SubmitReviewRequest struct {
	CommitID domain.HeadSHA
	Body     string
	Event    domain.ReviewDecision
	Comments []InlineComment
}

// CheckRun is one GitHub check run tied to a head commit.
type CheckRun struct {
	ID         int64
	Name       string
	Head       domain.HeadSHA
	Status     string
	Conclusion string
}

// ReviewThread is one pull request review thread with its root comment.
type ReviewThread struct {
	NodeID             string
	Resolved           bool
	Outdated           bool
	ViewerCanResolve   bool
	ViewerCanUnresolve bool
	RootComment        domain.ReviewComment
	// Replies are the comments under the root, in thread order. They carry the
	// author's response to a finding, which reconciliation shows the model.
	Replies []domain.ReviewComment
}

// APIError is a sanitized GitHub API failure with an HTTP status code.
//
// It reports a request GitHub answered and refused. A request that never
// reached GitHub has neither a status nor a message field, so it is reported
// instead by wrapping the transport error, which keeps the cause and keeps
// [errors.Is] and [errors.As] working through this package: internal/review reads
// [context.DeadlineExceeded] that way to title a check run. Neither error is fit
// for a pull request comment, and no caller publishes one; the sanitizing here
// bounds what reaches the service log.
type APIError struct {
	StatusCode int
	Message    string
}

// Error returns a sanitized GitHub API failure message.
func (err APIError) Error() string {
	return fmt.Sprintf("github api status %d: %s", err.StatusCode, err.Message)
}

// Client performs authenticated GitHub App REST and GraphQL requests.
type Client struct {
	cfg        config.Config
	httpClient *http.Client
	now        func() time.Time
	logger     *slog.Logger
	tokens     *tokenCache
}

// NewClient constructs a GitHub App client with installation token caching.
func NewClient(
	cfg config.Config,
	httpClient *http.Client,
	now func() time.Time,
	logger *slog.Logger,
) *Client {
	return &Client{
		cfg:        cfg,
		httpClient: httpClient,
		now:        now,
		logger:     logger,
		tokens:     newTokenCache(now),
	}
}

func (client *Client) restURL(path string) string {
	return strings.TrimRight(client.cfg.GitHubAPIBaseURL.String(), "/") + path
}

func (client *Client) repoPath(repo domain.Repository, suffix string) string {
	return fmt.Sprintf(
		"/repos/%s/%s%s",
		url.PathEscape(repo.Owner),
		url.PathEscape(repo.Name),
		suffix,
	)
}

func (client *Client) doREST(
	ctx context.Context,
	installationID int64,
	method string,
	path string,
	query url.Values,
	body json.RawMessage,
) ([]byte, error) {
	target := client.restURL(path)
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var payload io.Reader
	if len(body) > 0 {
		payload = bytes.NewReader(body)
	}

	token, err := client.installationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		client.logger.ErrorContext(ctx, "create github request", slog.String("err", err.Error()))
		return nil, errors.New("create github request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", githubAcceptHeader)
	request.Header.Set(githubAPIVersionHeader, githubAPIVersionValue)
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := client.httpClient.Do(request)
	if err != nil {
		client.logger.ErrorContext(ctx, "github request failed", slog.String("err", err.Error()))
		return nil, fmt.Errorf("github request failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	responseBody, err := readLimitedBody(response.Body)
	if err != nil {
		client.logger.ErrorContext(ctx, "read github response", slog.String("err", err.Error()))
		return nil, errors.New("read github response")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, newAPIError(response.StatusCode, sanitizeErrorBody(responseBody))
	}
	return responseBody, nil
}

func (client *Client) doRESTPaginated(
	ctx context.Context,
	installationID int64,
	path string,
	decodePage func([]byte) (int, error),
) error {
	nextURL := client.restURL(path)

	for nextURL != "" {
		token, err := client.installationToken(ctx, installationID)
		if err != nil {
			return err
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, http.NoBody)
		if err != nil {
			client.logger.ErrorContext(ctx, "create github request", slog.String("err", err.Error()))
			return errors.New("create github request")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Accept", githubAcceptHeader)
		request.Header.Set(githubAPIVersionHeader, githubAPIVersionValue)

		response, err := client.httpClient.Do(request)
		if err != nil {
			client.logger.ErrorContext(ctx, "github request failed", slog.String("err", err.Error()))
			return fmt.Errorf("github request failed: %w", err)
		}

		responseBody, readErr := readLimitedBody(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil {
			client.logger.ErrorContext(ctx, "read github response", slog.String("err", readErr.Error()))
			return errors.New("read github response")
		}
		if closeErr != nil {
			client.logger.ErrorContext(ctx, "close github response", slog.String("err", closeErr.Error()))
			return errors.New("close github response")
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return newAPIError(response.StatusCode, sanitizeErrorBody(responseBody))
		}

		if _, err := decodePage(responseBody); err != nil {
			return err
		}

		next, ok := parseNextLink(response.Header.Get("Link"))
		if !ok {
			nextURL = ""
			continue
		}
		nextURL = next
	}
	return nil
}

func (client *Client) doGraphQL(
	ctx context.Context,
	installationID int64,
	query string,
	variables json.RawMessage,
) ([]byte, error) {
	token, err := client.installationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}

	payload := graphQLRequest{
		Query:     query,
		Variables: variables,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		client.logger.ErrorContext(ctx, "marshal graphql request", slog.String("err", err.Error()))
		return nil, errors.New("marshal graphql request")
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.cfg.GitHubGraphQLURL.String(),
		bytes.NewReader(encoded),
	)
	if err != nil {
		client.logger.ErrorContext(ctx, "create graphql request", slog.String("err", err.Error()))
		return nil, errors.New("create graphql request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", githubAcceptHeader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(githubAPIVersionHeader, githubAPIVersionValue)

	response, err := client.httpClient.Do(request)
	if err != nil {
		client.logger.ErrorContext(ctx, "graphql request failed", slog.String("err", err.Error()))
		return nil, fmt.Errorf("graphql request failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	responseBody, err := readLimitedBody(response.Body)
	if err != nil {
		client.logger.ErrorContext(ctx, "read graphql response", slog.String("err", err.Error()))
		return nil, errors.New("read graphql response")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, newAPIError(response.StatusCode, sanitizeErrorBody(responseBody))
	}

	var envelope graphQLEnvelope
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		client.logger.ErrorContext(ctx, "decode graphql response", slog.String("err", err.Error()))
		return nil, errors.New("decode graphql response")
	}
	if len(envelope.Errors) > 0 {
		return nil, errors.New(formatGraphQLErrors(envelope.Errors))
	}
	return responseBody, nil
}

type graphQLRequest struct {
	Query     string          `json:"query"`
	Variables json.RawMessage `json:"variables"`
}

type graphQLError struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Path    json.RawMessage `json:"path"`
}

type graphQLEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors"`
}

func formatGraphQLErrors(errorsList []graphQLError) string {
	messages := make([]string, 0, len(errorsList))
	for _, item := range errorsList {
		details := make([]string, 0, 2)
		if item.Type != "" {
			details = append(details, "type="+item.Type)
		}
		if path := strings.TrimSpace(string(item.Path)); path != "" && path != "null" {
			details = append(details, "path="+path)
		}
		message := item.Message
		if len(details) > 0 {
			message += " (" + strings.Join(details, ", ") + ")"
		}
		messages = append(messages, message)
	}
	return strings.Join(messages, "; ")
}

func readLimitedBody(reader io.Reader) ([]byte, error) {
	limited := io.LimitReader(reader, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("read response body")
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, errors.New("response body exceeds limit")
	}
	return body, nil
}

func newAPIError(statusCode int, message string) APIError {
	return APIError{
		StatusCode: statusCode,
		Message:    message,
	}
}

func sanitizeErrorBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "request failed"
	}
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	for _, marker := range []string{"Bearer ", "ghs_", "ghp_", "github_pat_"} {
		if index := strings.Index(text, marker); index >= 0 {
			text = text[:index] + marker + "[redacted]"
		}
	}
	return text
}

func parseNextLink(linkHeader string) (string, bool) {
	if strings.TrimSpace(linkHeader) == "" {
		return "", false
	}
	for part := range strings.SplitSeq(linkHeader, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end <= start {
			continue
		}
		return part[start+1 : end], true
	}
	return "", false
}

func parseHeadSHA(value string) (domain.HeadSHA, error) {
	head, err := domain.ParseHeadSHA(value)
	if err != nil {
		return "", errors.New("invalid head sha")
	}
	return head, nil
}

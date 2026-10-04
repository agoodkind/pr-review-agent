// Package cloudflareops captures Cloudflare telemetry without printing credentials or log payloads.
package cloudflareops

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
	"os"
	"path/filepath"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/clock"
)

// APIURL uses one origin for telemetry queries and temporary-token revocation.
const (
	APIURL                  = "https://api.cloudflare.com/client/v4"
	observabilityPermission = "66c1ed49f4ed46098b75696a6d4ee3c9"
	maximumResponseBytes    = 32 * 1024 * 1024
)

type apiError struct {
	Code int `json:"code"`
}
type envelope struct {
	Success *bool           `json:"success"`
	Result  json.RawMessage `json:"result"`
	Errors  []apiError      `json:"errors"`
}

// Client excludes response bodies and credential values from returned errors.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient disables redirects because another host must not receive the bearer token.
func NewClient(baseURL string, token string, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("cloudflare API URL is invalid")
	}
	loopback := parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1"
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopback) {
		return nil, errors.New("cloudflare API URL must use HTTPS or a loopback test server")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("cloudflare credential is empty")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 45 * time.Second}
	}
	copyClient := *httpClient
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: strings.TrimSpace(token), httpClient: &copyClient}, nil
}

func (client *Client) request(ctx context.Context, method string, path string, payload []byte) (envelope, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return envelope{}, 0, errors.New("cloudflare request could not be created")
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		slog.WarnContext(ctx, "Cloudflare transport failed")
		return envelope{}, 0, fmt.Errorf("cloudflare transport failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil {
		return envelope{}, response.StatusCode, errors.New("cloudflare response could not be read")
	}
	if len(body) > maximumResponseBytes {
		return envelope{}, response.StatusCode, errors.New("cloudflare response exceeds the byte limit")
	}
	var decoded envelope
	if json.Unmarshal(body, &decoded) != nil || decoded.Success == nil {
		slog.WarnContext(ctx, "Cloudflare response envelope failed validation", "status", response.StatusCode)
		return envelope{}, response.StatusCode, fmt.Errorf("cloudflare returned an invalid response envelope with HTTP %d", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !*decoded.Success {
		codes := make([]int, 0, len(decoded.Errors))
		for _, item := range decoded.Errors {
			codes = append(codes, item.Code)
		}
		slog.WarnContext(ctx, "Cloudflare rejected the request", "status", response.StatusCode, "codes", codes)
		return decoded, response.StatusCode, fmt.Errorf("cloudflare rejected the request with HTTP %d and error codes %v", response.StatusCode, codes)
	}
	return decoded, response.StatusCode, nil
}

// Timeframe uses UTC milliseconds for both telemetry bounds.
type Timeframe struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// Filter includes the field type because Cloudflare's query API requires it.
type Filter struct {
	Key       string `json:"key"`
	Operation string `json:"operation"`
	Type      string `json:"type"`
	Value     string `json:"value"`
}

// Parameters supports a dataset selector because omitted selectors query all available datasets.
type Parameters struct {
	Datasets          []string `json:"datasets,omitempty"`
	Filters           []Filter `json:"filters,omitempty"`
	FilterCombination string   `json:"filterCombination,omitempty"`
}

// QueryRequest does not assume pagination semantics for native views other than events.
type QueryRequest struct {
	QueryID    string          `json:"queryId"`
	Timeframe  Timeframe       `json:"timeframe"`
	Parameters json.RawMessage `json:"parameters"`
	Limit      int             `json:"limit"`
	View       string          `json:"view"`
}

// Query returns one native telemetry view without treating API errors as empty results.
func (client *Client) Query(ctx context.Context, account string, query QueryRequest) (json.RawMessage, error) {
	if account == "" || strings.ContainsAny(account, "/?#") {
		return nil, errors.New("cloudflare account ID is invalid")
	}
	if query.Timeframe.From >= query.Timeframe.To {
		return nil, errors.New("telemetry query requires an increasing time range")
	}
	body, err := json.Marshal(query)
	if err != nil {
		return nil, errors.New("telemetry query could not be encoded")
	}
	response, _, err := client.request(ctx, http.MethodPost, "/accounts/"+account+"/workers/observability/telemetry/query", body)
	if err != nil {
		return nil, err
	}
	if len(response.Result) == 0 || bytes.Equal(response.Result, []byte("null")) {
		return nil, errors.New("cloudflare query omitted its result")
	}
	return response.Result, nil
}

type permissionGroup struct {
	ID string `json:"id"`
}
type tokenPolicy struct {
	Effect           string            `json:"effect"`
	PermissionGroups []permissionGroup `json:"permission_groups"`
	Resources        map[string]string `json:"resources"`
}
type tokenRequest struct {
	Name      string        `json:"name"`
	ExpiresOn string        `json:"expires_on"`
	Policies  []tokenPolicy `json:"policies"`
}
type tokenResult struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// CredentialOptions separates caller-managed credentials from expiring minted tokens.
type CredentialOptions struct {
	TokenFile        string
	AccountTokenFile string
	Account          string
	TTL              time.Duration
	APIURL           string
	PermissionName   string
}

// Credentials retains recovery metadata until revocation and rejection are verified.
type Credentials struct {
	Client        *Client
	accountClient *Client
	id            string
	directory     string
	Revoked       bool
}

// ReadCredential excludes file contents from diagnostics, including read failures.
func ReadCredential(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("Credential file could not be read", "path", path)
		return nil, fmt.Errorf("read credential file: %w", err)
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, errors.New("credential file is empty")
	}
	return data, nil
}

// AcquireCredentials revokes only tokens created by the command, including after canceled acquisitions.
func AcquireCredentials(ctx context.Context, options CredentialOptions) (credentials *Credentials, returnErr error) {
	slog.InfoContext(ctx, "Cloudflare credential acquisition started", "ephemeral", options.AccountTokenFile != "")
	if (options.TokenFile == "") == (options.AccountTokenFile == "") {
		return nil, errors.New("provide exactly one scoped token file or account token file")
	}
	if options.APIURL == "" {
		options.APIURL = APIURL
	}
	path := options.TokenFile
	if path == "" {
		path = options.AccountTokenFile
	}
	token, err := ReadCredential(path)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(options.APIURL, string(token), nil)
	if err != nil {
		return nil, err
	}
	var handle Credentials
	credentials = &handle
	credentials.Client = client
	if options.TokenFile != "" {
		return credentials, nil
	}
	if options.TTL <= 0 || options.TTL > 24*time.Hour {
		return nil, errors.New("temporary credential TTL must be positive and no greater than 24 hours")
	}
	if options.Account == "" || strings.ContainsAny(options.Account, "/?#") {
		return nil, errors.New("cloudflare account ID is invalid")
	}
	credentials.accountClient = client
	credentials.directory, err = os.MkdirTemp("", "pragent-ops-credentials.")
	if err != nil {
		slog.WarnContext(ctx, "Private credential directory creation failed")
		return nil, fmt.Errorf("create private credential directory: %w", err)
	}
	acquired := credentials
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, acquired.Close(ctx))
		}
	}()
	body, err := client.tokenBody(ctx, options)
	if err != nil {
		return nil, err
	}
	created, err := client.createToken(ctx, body)
	if err != nil {
		return nil, err
	}
	var scoped tokenResult
	if json.Unmarshal(created.Result, &scoped) != nil {
		return nil, errors.New("cloudflare returned an invalid scoped credential")
	}
	credentials.id = scoped.ID
	if scoped.ID == "" || scoped.Value == "" {
		return nil, errors.New("cloudflare did not return a complete scoped credential")
	}
	credentials.Client, err = NewClient(options.APIURL, scoped.Value, nil)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(credentials.directory, "token"), []byte(scoped.Value), 0o600); err != nil {
		slog.WarnContext(ctx, "Private temporary credential write failed")
		return nil, fmt.Errorf("write private temporary credential: %w", err)
	}
	if err = os.WriteFile(filepath.Join(credentials.directory, "id"), []byte(scoped.ID), 0o600); err != nil {
		slog.WarnContext(ctx, "Temporary credential identifier write failed")
		return nil, fmt.Errorf("write temporary credential identifier: %w", err)
	}
	_, _, err = credentials.Client.request(ctx, http.MethodGet, "/user/tokens/verify", nil)
	if err != nil {
		slog.WarnContext(ctx, "Scoped credential verification failed")
		return nil, fmt.Errorf("scoped credential verification failed: %w", err)
	}
	return credentials, nil
}

func (client *Client) tokenBody(ctx context.Context, options CredentialOptions) ([]byte, error) {
	permissionID, err := client.permissionID(ctx, options.PermissionName)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(tokenRequest{Name: "pragent-ops-temporary", ExpiresOn: clock.System().UTC().Add(options.TTL).Format(time.RFC3339), Policies: []tokenPolicy{{Effect: "allow", PermissionGroups: []permissionGroup{{ID: permissionID}}, Resources: map[string]string{"com.cloudflare.api.account." + options.Account: "*"}}}})
	if err != nil {
		slog.WarnContext(ctx, "Temporary token policy encoding failed")
		return nil, fmt.Errorf("encode temporary token policy: %w", err)
	}
	return body, nil
}

func (client *Client) permissionID(ctx context.Context, name string) (string, error) {
	if name == "" {
		return observabilityPermission, nil
	}
	response, _, err := client.request(ctx, http.MethodGet, "/user/tokens/permission_groups", nil)
	if err != nil {
		return "", err
	}
	var groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(response.Result, &groups); err != nil {
		return "", errors.New("cloudflare returned invalid permission groups")
	}
	var identifier string
	for _, group := range groups {
		if group.Name == name {
			if identifier != "" || group.ID == "" {
				return "", errors.New("cloudflare permission group is ambiguous")
			}
			identifier = group.ID
		}
	}
	if identifier == "" {
		return "", fmt.Errorf("cloudflare permission group %q is unavailable", name)
	}
	return identifier, nil
}

func (client *Client) createToken(ctx context.Context, body []byte) (envelope, error) {
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "Credential acquisition canceled")
		return envelope{}, fmt.Errorf("credential acquisition canceled: %w", err)
	}
	// Caller cancellation must not interrupt creation before the response provides the revocation ID.
	creationContext, cancelCreation := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancelCreation()
	created, _, err := client.request(creationContext, http.MethodPost, "/user/tokens", body)
	return created, err
}

// Close uses an independent context so cancellation cannot skip credential revocation.
func (credentials *Credentials) Close(ctx context.Context) error {
	if credentials == nil || credentials.directory == "" {
		return nil
	}
	if credentials.id != "" && !credentials.Revoked {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		_, _, err := credentials.accountClient.request(ctx, http.MethodDelete, "/user/tokens/"+credentials.id, nil)
		if err != nil {
			slog.WarnContext(ctx, "Temporary credential revocation failed", "id", credentials.id)
			return fmt.Errorf("credential %s revocation failed; credential directory %s requires recovery: %w", credentials.id, credentials.directory, err)
		}
		verified, status, verifyErr := credentials.Client.request(ctx, http.MethodGet, "/user/tokens/verify", nil)
		if verifyErr == nil || verified.Success == nil || *verified.Success || status >= 500 || len(verified.Errors) == 0 {
			return fmt.Errorf("credential %s revocation could not be verified; credential directory %s requires recovery", credentials.id, credentials.directory)
		}
		credentials.Revoked = true
		slog.InfoContext(ctx, "Temporary credential revocation verified", "id", credentials.id)
	}
	if err := os.RemoveAll(credentials.directory); err != nil {
		slog.WarnContext(ctx, "Temporary credential directory removal failed", "path", credentials.directory)
		return fmt.Errorf("remove temporary credential directory: %w", err)
	}
	credentials.directory = ""
	return nil
}

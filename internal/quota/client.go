package quota

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/telemetry"
)

const (
	maximumResponseBytes = 256 * 1024
	maximumPageSize      = 500
)

// QueryRequest uses endpoint flags to distinguish rolling and fixed boundaries.
type QueryRequest struct {
	Action           string `json:"action"`
	ProviderID       string `json:"provider_id"`
	Model            string `json:"model"`
	StartMS          int64  `json:"start_ms"`
	EndMS            int64  `json:"end_ms"`
	IncludeStart     bool   `json:"include_start"`
	IncludeEnd       bool   `json:"include_end"`
	AfterSequence    int64  `json:"after_sequence"`
	SnapshotSequence int64  `json:"snapshot_sequence"`
	PageSize         int    `json:"page_size"`
}

// QueryResponse retains the first page's sequence snapshot across pagination.
type QueryResponse struct {
	Events           []Event `json:"events"`
	SnapshotSequence int64   `json:"snapshot_sequence"`
	NextSequence     int64   `json:"next_sequence"`
	HistoryStartMS   int64   `json:"history_start_ms"`
}

type dailyRequest struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
	Limit      int64  `json:"limit"`
}

// DailySnapshot may omit counters when the legacy service returns only a decision.
type DailySnapshot struct {
	Allowed   bool   `json:"allowed"`
	Day       string `json:"day"`
	Used      *int64 `json:"used"`
	Limit     *int64 `json:"limit"`
	Remaining *int64 `json:"remaining"`
}

type reportResponse struct {
	Reported bool `json:"reported"`
}

// ReportRequest updates legacy counters with the original admission day.
type ReportRequest struct {
	ReportID     string `json:"report_id,omitempty"`
	Action       string `json:"action"`
	ProviderID   string `json:"provider_id"`
	Model        string `json:"model"`
	AdmittedAtMS int64  `json:"admitted_at_ms"`
	Usage
	LegacyDay    string `json:"legacy_day,omitempty"`
	LegacyTokens *int64 `json:"legacy_tokens,omitempty"`
}

// Client uses server enforcement to reject writes authenticated with an operator credential.
type Client struct {
	url           string
	signingKey    []byte
	operatorToken string
	httpClient    *http.Client
	reportPolicy  ReportPolicy
}

// NewClient requires one endpoint for legacy daily checks and timestamped usage.
func NewClient(url string, signingKey []byte, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{url: url, signingKey: signingKey, operatorToken: "", httpClient: httpClient, reportPolicy: DefaultReportPolicy()}
}

// NewReportingClient rejects invalid delivery settings before sending usage.
func NewReportingClient(endpoint string, signingKey []byte, httpClient *http.Client, policy ReportPolicy) (*Client, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	client := NewClient(endpoint, signingKey, httpClient)
	client.reportPolicy = policy
	return client, nil
}

// NewOperatorClient disables redirects because another origin must not receive the operator credential.
func NewOperatorClient(endpoint string, token []byte, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("operator counter URL is invalid")
	}
	loopback := parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1"
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopback) {
		return nil, errors.New("operator counter URL must use HTTPS or a loopback test server")
	}
	if strings.TrimSpace(string(token)) == "" {
		return nil, errors.New("operator token is empty")
	}
	client := NewClient(endpoint, nil, httpClient)
	client.operatorToken = string(token)
	copyHTTPClient := *client.httpClient
	copyHTTPClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	client.httpClient = &copyHTTPClient
	return client, nil
}

// CheckDaily uses legacy UTC daily totals, which contain no request timestamps.
func (client *Client) CheckDaily(ctx context.Context, providerID, model string, limit int64) (DailySnapshot, error) {
	var result DailySnapshot
	err := post(ctx, client, dailyRequest{ProviderID: providerID, Model: model, Limit: limit}, &result)
	return result, err
}

// Query excludes reports inserted after the first page's sequence snapshot.
func (client *Client) Query(ctx context.Context, providerID, model string, bounds Bounds) (QueryResponse, error) {
	request := QueryRequest{
		Action: "query_usage", ProviderID: providerID, Model: model,
		StartMS: bounds.StartMS, EndMS: bounds.EndMS, IncludeStart: bounds.IncludeStart,
		IncludeEnd: bounds.IncludeEnd, PageSize: maximumPageSize, AfterSequence: 0, SnapshotSequence: 0,
	}
	var result QueryResponse
	first := true
	for {
		var page QueryResponse
		if err := post(ctx, client, request, &page); err != nil {
			return QueryResponse{}, err
		}
		if page.SnapshotSequence < 0 || page.NextSequence < 0 || page.HistoryStartMS <= 0 || len(page.Events) > maximumPageSize {
			return QueryResponse{}, errors.New("accounting service returned invalid page metadata")
		}
		if !first && (page.SnapshotSequence != result.SnapshotSequence || page.HistoryStartMS != result.HistoryStartMS) {
			return QueryResponse{}, errors.New("accounting service changed the usage snapshot")
		}
		result.SnapshotSequence = page.SnapshotSequence
		result.HistoryStartMS = page.HistoryStartMS
		previous := request.AfterSequence
		for _, event := range page.Events {
			if event.Sequence <= previous || event.Sequence > page.SnapshotSequence || !bounds.contains(event.AdmittedAtMS) || !validUsage(event.Usage) {
				return QueryResponse{}, errors.New("accounting service returned invalid usage")
			}
			previous = event.Sequence
			result.Events = append(result.Events, event)
		}
		if page.NextSequence == 0 {
			return result, nil
		}
		if len(page.Events) == 0 || page.NextSequence != previous || page.NextSequence <= request.AfterSequence {
			return QueryResponse{}, errors.New("accounting service returned a nonadvancing cursor")
		}
		request.AfterSequence = page.NextSequence
		request.SnapshotSequence = page.SnapshotSequence
		first = false
	}
}

// Report retains one identity across transient failures because the response may fail after storage commits.
func (client *Client) Report(ctx context.Context, request ReportRequest) error {
	ctx, cancelReport := context.WithTimeout(ctx, client.reportPolicy.Timeout)
	defer cancelReport()
	request.Action = "report_usage"
	if request.ReportID == "" {
		request.ReportID = rand.Text()
	}
	if !validUsage(request.Usage) || request.AdmittedAtMS < 0 || request.AdmittedAtMS > MaximumInteger {
		return errors.New("usage report is outside the storage integer range")
	}
	for attempt := range client.reportPolicy.Attempts {
		var response reportResponse
		attemptContext, cancel := context.WithTimeout(ctx, client.reportPolicy.AttemptTimeout)
		err := post(attemptContext, client, request, &response)
		cancel()
		if err == nil {
			if !response.Reported {
				return errors.New("accounting service did not acknowledge usage")
			}
			return nil
		}
		if ctx.Err() != nil || attempt == client.reportPolicy.Attempts-1 || !retryableReportError(err) {
			return err
		}
	}
	return nil
}

type storageStatusError struct {
	status int
}

func (err *storageStatusError) Error() string {
	return fmt.Sprintf("accounting service returned HTTP %d", err.status)
}

func retryableReportError(err error) bool {
	var status *storageStatusError
	if errors.As(err, &status) {
		return status.status == http.StatusRequestTimeout || status.status == http.StatusTooManyRequests || status.status >= http.StatusInternalServerError
	}
	var transport *url.Error
	return errors.As(err, &transport) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF)
}

func validUsage(usage Usage) bool {
	for _, count := range []*int64{usage.CachedInputTokens, usage.CacheWriteTokens} {
		if count != nil && (*count < 0 || *count > usage.InputTokens) {
			return false
		}
	}
	if usage.CachedInputTokens != nil && usage.CacheWriteTokens != nil && *usage.CachedInputTokens > usage.InputTokens-*usage.CacheWriteTokens {
		return false
	}
	if len(usage.ServiceTier) > 200 {
		return false
	}
	return usage.InputTokens >= 0 && usage.InputTokens <= MaximumInteger &&
		usage.OutputTokens >= 0 && usage.OutputTokens <= MaximumInteger &&
		usage.TotalTokens >= 0 && usage.TotalTokens <= MaximumInteger
}

func post[Request QueryRequest | ReportRequest | dailyRequest, Response QueryResponse | reportResponse | DailySnapshot](ctx context.Context, client *Client, payload Request, result *Response) error {
	logger := gklog.L(ctx)
	body, err := json.Marshal(payload)
	if err != nil {
		logger.WarnContext(ctx, "quota storage request encoding failed", slog.String("err", err.Error()))
		return fmt.Errorf("encode quota storage request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.url, bytes.NewReader(body))
	if err != nil {
		logger.WarnContext(ctx, "quota storage request creation failed", slog.String("err", err.Error()))
		return fmt.Errorf("create quota storage request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if client.operatorToken != "" {
		request.Header.Set("Authorization", "Bearer "+client.operatorToken)
	} else {
		request.Header.Set("X-Pr-Agent-Budget-Signature", telemetry.Sign(client.signingKey, body))
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		logger.WarnContext(ctx, "quota storage request failed", slog.String("err", err.Error()))
		return fmt.Errorf("send quota storage request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return &storageStatusError{status: response.StatusCode}
	}
	limited := io.LimitReader(response.Body, maximumResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		logger.WarnContext(ctx, "quota storage response read failed", slog.String("err", err.Error()))
		return fmt.Errorf("read quota storage response: %w", err)
	}
	if len(data) > maximumResponseBytes {
		return errors.New("accounting response exceeds its size limit")
	}
	if err := json.Unmarshal(data, result); err != nil {
		logger.WarnContext(ctx, "quota storage response decoding failed", slog.String("err", err.Error()))
		return fmt.Errorf("decode quota storage response: %w", err)
	}
	return nil
}

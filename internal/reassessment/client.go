// Package reassessment persists unfinished reviews and schedules eligible retries.
package reassessment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/telemetry"
)

// SignatureHeader authenticates service mutations independently of operator reads.
const (
	SignatureHeader = "X-Pr-Agent-Reassessment-Signature"
	// CallbackPath permits only signed planning and execution requests.
	CallbackPath = "/internal/v1/reassessment"
	// QueuePath also accepts authenticated operator snapshots without mutation access.
	QueuePath = "/internal/v1/reassessments"
)

// Client signs queue mutations with the existing webhook key.
type Client struct {
	URL        string
	SigningKey []byte
	HTTP       *http.Client
}

// Mutation rejects updates when the stored version differs from ExpectedVersion.
type Mutation struct {
	Action          string  `json:"action"`
	Key             string  `json:"key"`
	ExpectedVersion int64   `json:"expected_version"`
	Record          *Record `json:"record,omitempty"`
}

// Response reports concurrent update rejection separately from transport failure.
type Response struct {
	Applied bool     `json:"applied"`
	Record  *Record  `json:"record"`
	Records []Record `json:"records,omitempty"`
}

// Send requires a storage acknowledgment before review delivery settlement.
func (client *Client) Send(ctx context.Context, mutation Mutation) (Response, error) {
	logger := gklog.L(ctx)
	var result Response
	body, err := json.Marshal(mutation)
	if err != nil {
		logger.WarnContext(ctx, "encode reassessment mutation", slog.String("err", err.Error()))
		return result, fmt.Errorf("encode reassessment mutation: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.URL, bytes.NewReader(body))
	if err != nil {
		logger.WarnContext(ctx, "create reassessment request", slog.String("err", err.Error()))
		return result, fmt.Errorf("create reassessment request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(SignatureHeader, telemetry.Sign(client.SigningKey, body))
	response, err := client.HTTP.Do(request)
	if err != nil {
		logger.WarnContext(ctx, "reassessment storage request", slog.String("err", err.Error()))
		return result, fmt.Errorf("reassessment storage request: %w", err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			logger.WarnContext(ctx, "close reassessment response", slog.String("err", closeErr.Error()))
		}
	}()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("reassessment storage returned HTTP %d", response.StatusCode)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if readErr != nil {
		logger.WarnContext(ctx, "read reassessment record", slog.String("err", readErr.Error()))
		return result, fmt.Errorf("read reassessment record: %w", readErr)
	}
	var fields map[string]json.RawMessage
	if shapeErr := json.Unmarshal(data, &fields); shapeErr != nil || fields == nil || len(fields["record"]) == 0 || len(fields["applied"]) == 0 {
		return result, fmt.Errorf("reassessment storage response is incomplete")
	}
	if err := json.Unmarshal(data, &result); err != nil {
		logger.WarnContext(ctx, "decode reassessment record", slog.String("err", err.Error()))
		return result, fmt.Errorf("decode reassessment record: %w", err)
	}
	return result, nil
}

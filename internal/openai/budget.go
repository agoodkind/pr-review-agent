package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"goodkind.io/pr-review-agent/internal/telemetry"
)

const (
	budgetRequestTimeout = 5 * time.Second
)

type budgetAdmissionError struct {
	providerID string
	cause      error
}

func (budgetError *budgetAdmissionError) Error() string {
	return fmt.Sprintf("provider %s daily token admission failed: %v", budgetError.providerID, budgetError.cause)
}

func (budgetError *budgetAdmissionError) Unwrap() error {
	return budgetError.cause
}

func (client *Client) checkBudget(ctx context.Context, target provider) (string, error) {
	if target.dailyTokenLimit == 0 {
		return "", nil
	}
	if client.budgetURL == "" || len(client.budgetSigningKey) == 0 {
		return "", &budgetAdmissionError{providerID: target.id, cause: errors.New("budget service is not configured")}
	}
	body, err := json.Marshal(struct {
		ProviderID string `json:"provider_id"`
		Limit      int64  `json:"limit"`
	}{ProviderID: target.id, Limit: target.dailyTokenLimit})
	if err != nil {
		return "", &budgetAdmissionError{providerID: target.id, cause: err}
	}
	requestContext, cancel := context.WithTimeout(ctx, budgetRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, client.budgetURL, bytes.NewReader(body))
	if err != nil {
		return "", &budgetAdmissionError{providerID: target.id, cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pr-Agent-Budget-Signature", telemetry.Sign(client.budgetSigningKey, body))
	response, err := client.httpClient.Do(request)
	if err != nil {
		return "", &budgetAdmissionError{providerID: target.id, cause: err}
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return "", &budgetAdmissionError{providerID: target.id, cause: fmt.Errorf("budget service returned HTTP %d", response.StatusCode)}
	}
	var decision struct {
		Allowed bool   `json:"allowed"`
		Day     string `json:"day"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1024)).Decode(&decision); err != nil {
		return "", &budgetAdmissionError{providerID: target.id, cause: err}
	}
	if !decision.Allowed {
		return "", &budgetAdmissionError{providerID: target.id, cause: errors.New("daily token limit exhausted")}
	}
	if decision.Day == "" {
		return "", &budgetAdmissionError{providerID: target.id, cause: errors.New("budget service omitted the UTC day")}
	}
	return decision.Day, nil
}

func (client *Client) reportBudget(ctx context.Context, target provider, day string, tokens int64) error {
	if target.dailyTokenLimit == 0 || tokens <= 0 {
		return nil
	}
	body, err := json.Marshal(struct {
		ProviderID string `json:"provider_id"`
		Day        string `json:"day"`
		Tokens     int64  `json:"tokens"`
	}{ProviderID: target.id, Day: day, Tokens: tokens})
	if err != nil {
		return err
	}
	requestContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), budgetRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, client.budgetURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pr-Agent-Budget-Signature", telemetry.Sign(client.budgetSigningKey, body))
	response, err := client.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("budget report returned HTTP %d", response.StatusCode)
	}
	return nil
}

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

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/telemetry"
)

const (
	reservationOverheadTokens = 16384
	budgetRequestTimeout      = 5 * time.Second
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

func (client *Client) reserveBudget(
	ctx context.Context,
	target provider,
	prompt string,
	policy string,
	schemaName string,
	schema json.RawMessage,
) error {
	if target.dailyTokenLimit == 0 {
		return nil
	}
	if client.budgetURL == "" || len(client.budgetSigningKey) == 0 {
		return &budgetAdmissionError{providerID: target.id, cause: errors.New("budget service is not configured")}
	}
	systemPrompt := structuredOutputPrompt(policy, schemaName, schema)
	reservation := int64(len(systemPrompt) + len(prompt) + len(schemaName) + len(schema) + config.MaximumOutputTokens + reservationOverheadTokens)
	body, err := json.Marshal(struct {
		ProviderID string `json:"provider_id"`
		Tokens     int64  `json:"tokens"`
		Limit      int64  `json:"limit"`
	}{ProviderID: target.id, Tokens: reservation, Limit: target.dailyTokenLimit})
	if err != nil {
		return &budgetAdmissionError{providerID: target.id, cause: err}
	}
	requestContext, cancel := context.WithTimeout(ctx, budgetRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, client.budgetURL, bytes.NewReader(body))
	if err != nil {
		return &budgetAdmissionError{providerID: target.id, cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pr-Agent-Budget-Signature", telemetry.Sign(client.budgetSigningKey, body))
	response, err := client.httpClient.Do(request)
	if err != nil {
		return &budgetAdmissionError{providerID: target.id, cause: err}
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return &budgetAdmissionError{providerID: target.id, cause: fmt.Errorf("budget service returned HTTP %d", response.StatusCode)}
	}
	var decision struct {
		Allowed bool `json:"allowed"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1024)).Decode(&decision); err != nil {
		return &budgetAdmissionError{providerID: target.id, cause: err}
	}
	if !decision.Allowed {
		return &budgetAdmissionError{providerID: target.id, cause: errors.New("daily token limit exhausted")}
	}
	return nil
}

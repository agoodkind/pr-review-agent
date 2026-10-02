package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/telemetry"
)

const (
	budgetRequestTimeout = 5 * time.Second
)

var errDailyTokenLimitExhausted = errors.New("daily token limit exhausted")

type budgetAdmissionError struct {
	providerID string
	model      string
	snapshot   budgetSnapshot
	cause      error
}

type budgetSnapshot struct {
	day       string
	used      int64
	limit     int64
	remaining int64
	known     bool
}

func (budgetError *budgetAdmissionError) Error() string {
	return fmt.Sprintf("provider %s model %s daily token admission failed: %v", budgetError.providerID, budgetError.model, budgetError.cause)
}

func (budgetError *budgetAdmissionError) Unwrap() error {
	return budgetError.cause
}

func (budgetError *budgetAdmissionError) UsageExceeded() bool {
	return errors.Is(budgetError.cause, errDailyTokenLimitExhausted)
}

func (budgetError *budgetAdmissionError) DailyBudgetExhausted() bool {
	return errors.Is(budgetError.cause, errDailyTokenLimitExhausted)
}

func (client *Client) checkBudget(ctx context.Context, target provider) (budgetSnapshot, error) {
	snapshot := budgetSnapshot{day: "", used: 0, limit: target.dailyTokenLimit, remaining: 0, known: false}
	if target.dailyTokenLimit == 0 {
		return snapshot, nil
	}
	if client.budgetURL == "" || len(client.budgetSigningKey) == 0 {
		return snapshot, budgetFailure(target, snapshot, errors.New("budget service is not configured"))
	}
	body, err := json.Marshal(struct {
		ProviderID string `json:"provider_id"`
		Model      string `json:"model"`
		Limit      int64  `json:"limit"`
	}{ProviderID: target.id, Model: target.model, Limit: target.dailyTokenLimit})
	if err != nil {
		return snapshot, budgetFailure(target, snapshot, err)
	}
	requestContext, cancel := context.WithTimeout(ctx, budgetRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, client.budgetURL, bytes.NewReader(body))
	if err != nil {
		return snapshot, budgetFailure(target, snapshot, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pr-Agent-Budget-Signature", telemetry.Sign(client.budgetSigningKey, body))
	response, err := client.httpClient.Do(request)
	if err != nil {
		return snapshot, budgetFailure(target, snapshot, err)
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return snapshot, budgetFailure(target, snapshot, fmt.Errorf("budget service returned HTTP %d", response.StatusCode))
	}
	var decision struct {
		Allowed   bool   `json:"allowed"`
		Day       string `json:"day"`
		Used      *int64 `json:"used"`
		Limit     *int64 `json:"limit"`
		Remaining *int64 `json:"remaining"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1024)).Decode(&decision); err != nil {
		return snapshot, budgetFailure(target, snapshot, err)
	}
	snapshot.day = decision.Day
	if decision.Used != nil && decision.Limit != nil && decision.Remaining != nil &&
		*decision.Used >= 0 && *decision.Limit == target.dailyTokenLimit && *decision.Remaining >= 0 {
		snapshot.used = *decision.Used
		snapshot.remaining = *decision.Remaining
		snapshot.known = true
	}
	if !decision.Allowed {
		return snapshot, budgetFailure(target, snapshot, errDailyTokenLimitExhausted)
	}
	if decision.Day == "" {
		return snapshot, budgetFailure(target, snapshot, errors.New("budget service omitted the UTC day"))
	}
	return snapshot, nil
}

func budgetFailure(target provider, snapshot budgetSnapshot, cause error) error {
	return &budgetAdmissionError{providerID: target.id, model: target.model, snapshot: snapshot, cause: cause}
}

func (client *Client) reportBudget(ctx context.Context, target provider, day string, tokens int64) {
	if target.dailyTokenLimit == 0 || tokens <= 0 {
		return
	}
	body, err := json.Marshal(struct {
		ProviderID string `json:"provider_id"`
		Model      string `json:"model"`
		Day        string `json:"day"`
		Tokens     int64  `json:"tokens"`
	}{ProviderID: target.id, Model: target.model, Day: day, Tokens: tokens})
	if err != nil {
		gklog.L(ctx).WarnContext(ctx, "encode model budget report failed", slog.String("err", err.Error()))
		return
	}
	requestContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), budgetRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, client.budgetURL, bytes.NewReader(body))
	if err != nil {
		gklog.L(ctx).WarnContext(ctx, "create model budget report request failed", slog.String("err", err.Error()))
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pr-Agent-Budget-Signature", telemetry.Sign(client.budgetSigningKey, body))
	response, err := client.httpClient.Do(request)
	if err != nil {
		gklog.L(ctx).WarnContext(ctx, "send model budget report failed", slog.String("err", err.Error()))
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		gklog.L(ctx).WarnContext(ctx, "model budget report rejected", slog.Int("status", response.StatusCode))
	}
}

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

	"github.com/openai/openai-go/v3/responses"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/telemetry"
)

const (
	budgetRequestTimeout = 5 * time.Second
)

var errDailyTokenLimitExhausted = errors.New("configured token limit exhausted")

type budgetAdmissionError struct {
	providerID string
	model      string
	snapshot   budgetSnapshot
	cause      error
}

type budgetSnapshot struct {
	day          string
	used         int64
	limit        int64
	remaining    int64
	known        bool
	admittedAtMS int64
	window       quota.Window
	accounting   quota.Snapshot
}

func (budgetError *budgetAdmissionError) Error() string {
	return fmt.Sprintf("provider %s model %s token admission failed: %v", budgetError.providerID, budgetError.model, budgetError.cause)
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
	now := client.now()
	var emptyWindow quota.Window
	var emptyAccounting quota.Snapshot
	snapshot := budgetSnapshot{day: "", used: 0, limit: target.dailyTokenLimit, remaining: 0, known: false, admittedAtMS: now.UnixMilli(), window: emptyWindow, accounting: emptyAccounting}
	if target.tokenLimit > 0 {
		return client.checkWindowBudget(ctx, target, now, snapshot)
	}
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
	if day, err := time.Parse("2006-01-02", decision.Day); err == nil {
		snapshot.accounting.Bounds = quota.Bounds{StartMS: day.UnixMilli(), EndMS: day.Add(24 * time.Hour).UnixMilli(), IncludeStart: true, IncludeEnd: false}
		if !decision.Allowed {
			snapshot.accounting.AvailableAtMS = snapshot.accounting.Bounds.EndMS
		}
	}
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

func (client *Client) checkWindowBudget(ctx context.Context, target provider, now time.Time, snapshot budgetSnapshot) (budgetSnapshot, error) {
	snapshot.limit, snapshot.window = target.tokenLimit, target.tokenWindow
	if client.budgetURL == "" || len(client.budgetSigningKey) == 0 {
		return snapshot, budgetFailure(target, snapshot, errors.New("budget service is not configured"))
	}
	bounds, err := target.tokenWindow.Bounds(now)
	if err != nil {
		return snapshot, budgetFailure(target, snapshot, err)
	}
	requestContext, cancel := context.WithTimeout(ctx, budgetRequestTimeout)
	defer cancel()
	storage := quota.NewClient(client.budgetURL, client.budgetSigningKey, client.httpClient)
	usage, err := storage.Query(requestContext, target.id, target.model, bounds)
	if err != nil {
		return snapshot, budgetFailure(target, snapshot, err)
	}
	accounting, err := quota.Evaluate(target.tokenWindow, now, target.tokenLimit, target.tokenTypes, usage.Events, usage.HistoryStartMS)
	if err != nil {
		return snapshot, budgetFailure(target, snapshot, err)
	}
	snapshot.accounting = accounting
	snapshot.used, snapshot.remaining, snapshot.known = accounting.Used, accounting.Remaining, true
	if !accounting.Allowed {
		return snapshot, budgetFailure(target, snapshot, errDailyTokenLimitExhausted)
	}
	return snapshot, nil
}

func budgetFailure(target provider, snapshot budgetSnapshot, cause error) error {
	return &budgetAdmissionError{providerID: target.id, model: target.model, snapshot: snapshot, cause: cause}
}

func (client *Client) reportBudget(ctx context.Context, target provider, snapshot budgetSnapshot, usage responses.ResponseUsage) {
	if client.budgetURL == "" || len(client.budgetSigningKey) == 0 {
		return
	}
	report := quota.ReportRequest{
		Action: "report_usage", ProviderID: target.id, Model: target.model,
		AdmittedAtMS: snapshot.admittedAtMS,
		Usage:        quota.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens}, LegacyDay: "", LegacyTokens: nil,
	}
	if target.dailyTokenLimit > 0 {
		tokens := budgetTokens(usage, target.dailyTokenTypes)
		report.LegacyDay, report.LegacyTokens = snapshot.day, &tokens
	}
	requestContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), budgetRequestTimeout)
	defer cancel()
	storage := quota.NewClient(client.budgetURL, client.budgetSigningKey, client.httpClient)
	if err := storage.Report(requestContext, report); err != nil {
		gklog.L(ctx).WarnContext(ctx, "send model budget report failed", slog.String("err", err.Error()))
	}
}

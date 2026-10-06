package openai

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/quota"
)

// NextAvailable reads every app quota without sending a model request.
func (client *Client) NextAvailable(ctx context.Context) (time.Time, error) {
	now := client.now()
	var earliest time.Time
	var failures []error
	for _, target := range client.providers {
		providerFailed := false
		var snapshot budgetSnapshot
		var readyAt time.Time
		check := func(checked budgetSnapshot, err error) {
			if err == nil {
				return
			}
			var admission *budgetAdmissionError
			if !errors.As(err, &admission) || !admission.DailyBudgetExhausted() {
				failures = append(failures, err)
				providerFailed = true
				return
			}
			at := time.UnixMilli(checked.accounting.AvailableAtMS)
			if at.After(readyAt) {
				readyAt = at
			}
		}
		if target.dailyTokenLimit > 0 {
			checked, err := client.checkDailyBudget(ctx, target, snapshot)
			check(checked, err)
		}
		limits := append([]quota.Limit{}, target.tokenLimits...)
		if target.tokenLimit > 0 {
			limits = append(limits, quota.Limit{Limit: target.tokenLimit, TokenTypes: target.tokenTypes, Window: target.tokenWindow})
		}
		for _, limit := range limits {
			checked, err := client.checkWindowBudget(ctx, target, now, snapshot, limit)
			check(checked, err)
		}
		if readyAt.IsZero() && !providerFailed {
			return time.Time{}, nil
		}
		if !providerFailed && !readyAt.IsZero() && (earliest.IsZero() || readyAt.Before(earliest)) {
			earliest = readyAt
		}
	}
	if !earliest.IsZero() {
		return earliest, nil
	}
	err := errors.Join(failures...)
	if err != nil {
		gklog.L(ctx).WarnContext(ctx, "read provider availability", slog.String("err", err.Error()))
	}
	return time.Time{}, err
}

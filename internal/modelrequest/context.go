// Package modelrequest separates provider attempt deadlines from parent cancellation.
package modelrequest

import (
	"context"
	"time"
)

// ProviderTimeoutManager permits independent deadlines during fallback.
type ProviderTimeoutManager interface {
	ManagesProviderTimeouts() bool
}

type timeoutKey struct{}

// WithTimeout stores the delivery timeout for independent provider deadlines.
// Models without provider deadlines receive one caller deadline.
func WithTimeout(ctx context.Context, manager ProviderTimeoutManager, timeout time.Duration) (context.Context, context.CancelFunc) {
	if manager != nil && manager.ManagesProviderTimeouts() {
		return context.WithCancel(context.WithValue(ctx, timeoutKey{}, timeout))
	}
	return context.WithTimeout(ctx, timeout)
}

// Timeout applies the delivery timeout when the container uses an older default.
func Timeout(ctx context.Context, fallback time.Duration) time.Duration {
	if timeout, ok := ctx.Value(timeoutKey{}).(time.Duration); ok && timeout > 0 {
		return timeout
	}
	return fallback
}

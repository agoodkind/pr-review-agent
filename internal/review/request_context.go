package review

import (
	"context"
	"time"

	"goodkind.io/pr-review-agent/internal/modelrequest"
)

func modelRequestContext(ctx context.Context, model Model, timeout time.Duration) (context.Context, context.CancelFunc) {
	manager, _ := model.(modelrequest.ProviderTimeoutManager)
	return modelrequest.WithTimeout(ctx, manager, timeout)
}

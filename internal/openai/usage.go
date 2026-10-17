package openai

import (
	"github.com/openai/openai-go/v3/responses"

	"goodkind.io/pr-review-agent/internal/quota"
)

type usageReporter func(responses.ResponseUsage, string, *quota.SDKUsage)

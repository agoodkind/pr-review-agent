package main

import (
	"net/http"

	"goodkind.io/pr-review-agent/internal/liveproof"
)

func init() {
	_ = liveproof.RequireAdmin(http.NotFoundHandler())
	_ = liveproof.RetryDelay(3)
}

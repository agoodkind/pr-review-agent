package main

import (
	"net/http"

	"goodkind.io/pr-review-agent/internal/liveproof"
)

func init() {
	_ = liveproof.RequireAdmin(http.NotFoundHandler())
	request := &http.Request{Header: http.Header{"X-Optional": {"proof"}}}
	_ = liveproof.OptionalHeader(request)
}

package main

import (
	"net/http"

	"goodkind.io/pr-review-agent/internal/liveproof"
)

func init() {
	_ = liveproof.Mean([]int{1, 2, 3})
	_ = liveproof.RequireAdmin(http.NotFoundHandler())
}

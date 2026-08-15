package main

import "goodkind.io/pr-review-agent/internal/liveproof"

func init() {
	_ = liveproof.Mean([]int{1, 2, 3})
}

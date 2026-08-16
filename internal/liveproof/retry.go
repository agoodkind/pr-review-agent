package liveproof

import "time"

// RetryDelay returns the backoff before a provider request is retried.
func RetryDelay(attempt int) time.Duration {
	return time.Duration(1 << attempt)
}

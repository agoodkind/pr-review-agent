package openai

import (
	"fmt"
	"net/http"
	"strings"
)

// usageExceededPhrases are the provider wordings that report exhausted usage.
// The gateway flattens every upstream status onto HTTP 400 and keeps the real
// upstream status and body only inside the message, so the message is the one
// reliable place to read an exhausted quota from. A bare upstream 429 is
// deliberately absent: the gateway returns it for plain throttling too.
var usageExceededPhrases = []string{
	"insufficient_quota",
	"usage_limit_reached",
	"usage_not_included",
	"billing_hard_limit_reached",
	"billing hard limit",
	"usage limit",
	"quota exceeded",
	"exceeded your current quota",
	"exceeded your usage",
	"out of credit",
	"credit balance",
	"upstream status 402",
}

// StreamError reports that a completion stream ended before the model finished
// answering, because the connection carrying it broke.
//
// The gateway reports a broken upstream stream as an error frame inside the
// stream itself, not as an HTTP status, so the SDK's own retry never sees it
// and the request is lost after the answer was already underway. Nothing about
// the request caused it, so sending the same request again is the recovery.
type StreamError struct {
	Model string
	Cause error
}

// Error states that the stream broke and keeps the underlying description,
// which is the only account of what broke.
func (streamError *StreamError) Error() string {
	return "model " + streamError.Model + " stream ended before the answer finished: " +
		streamError.Cause.Error()
}

// Unwrap exposes the underlying failure so a caller can still match on it.
func (streamError *StreamError) Unwrap() error {
	return streamError.Cause
}

// Retryable reports whether repeating the identical request can succeed.
//
// A broken connection is transient. An exhausted quota reported through the
// same stream is not, and repeating that request only spends the review's
// remaining time on a refusal it will get again.
func (streamError *StreamError) Retryable() bool {
	lowered := strings.ToLower(streamError.Cause.Error())
	for _, phrase := range usageExceededPhrases {
		if strings.Contains(lowered, phrase) {
			return false
		}
	}
	return true
}

// TruncatedError reports that the model stopped before finishing its answer
// because it reached the completion token budget. Reasoning and answer tokens
// share that budget, so a chunk that yields many findings can exhaust it. A
// caller can recover by reviewing less content per request.
type TruncatedError struct {
	Model string
}

// Error states that the answer stopped early and why.
func (truncatedError *TruncatedError) Error() string {
	return "model " + truncatedError.Model + " stopped before finishing its answer, " +
		"because it reached the completion token budget"
}

// Truncated identifies this failure to callers that cannot import this package,
// because this package already imports theirs.
func (truncatedError *TruncatedError) Truncated() bool {
	return true
}

// ProviderError is one structured failure reported by the model provider.
type ProviderError struct {
	StatusCode int
	Type       string
	Code       string
	Param      string
	Message    string
}

// Error renders the provider status and every non-empty detail it reported.
func (providerError *ProviderError) Error() string {
	details := []string{fmt.Sprintf(
		"model provider returned HTTP %d %s",
		providerError.StatusCode,
		http.StatusText(providerError.StatusCode),
	)}
	fields := []string{
		providerError.Type,
		providerError.Code,
		providerError.Param,
		providerError.Message,
	}
	for _, field := range fields {
		collapsed := strings.Join(strings.Fields(field), " ")
		if collapsed != "" {
			details = append(details, collapsed)
		}
	}
	return strings.Join(details, ": ")
}

// UsageExceeded reports whether the provider refused the request for lack of
// remaining usage rather than for a transient or request-shape problem.
func (providerError *ProviderError) UsageExceeded() bool {
	if providerError.StatusCode == http.StatusPaymentRequired {
		return true
	}
	lowered := strings.ToLower(providerError.Type + " " + providerError.Code + " " + providerError.Message)
	for _, phrase := range usageExceededPhrases {
		if strings.Contains(lowered, phrase) {
			return true
		}
	}
	return false
}

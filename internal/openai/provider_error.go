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

// causeError describes a model provider failure in plain words while keeping
// the underlying error reachable, so a caller can still match a sentinel such
// as [context.Canceled] and tell a restart apart from a real provider failure.
type causeError struct {
	description string
	cause       error
}

// Error states the description and the underlying error.
func (wrapped *causeError) Error() string {
	return wrapped.description + ": " + wrapped.cause.Error()
}

// Unwrap exposes the underlying error to [errors.Is] and [errors.As].
func (wrapped *causeError) Unwrap() error {
	return wrapped.cause
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

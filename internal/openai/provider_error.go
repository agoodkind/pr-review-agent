package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
// answering.
//
// The gateway reports an upstream failure two different ways for the same
// underlying refusal. Sometimes it answers with an HTTP status. Sometimes it
// accepts the request, opens the stream, and writes an error frame into it.
//
// A stream failure therefore carries the same structured provider error the
// HTTP path produces, parsed out of the frame, so one refusal is classified the
// same way whichever transport delivered it.
type StreamError struct {
	Model string
	Cause error
	// Provider is the structured error read out of the stream frame. It is nil
	// when the frame carried no recognizable provider error, which is what a
	// dropped connection looks like.
	Provider *ProviderError
}

// Error states that the stream broke and keeps the underlying description,
// which is the only account of what broke.
func (streamError *StreamError) Error() string {
	return "model " + streamError.Model + " stream ended before the answer finished: " +
		streamError.Cause.Error()
}

// Unwrap exposes the structured provider error when the frame carried one, so a
// caller matching on *ProviderError classifies both transports identically.
// Without a provider error it exposes the underlying transport failure.
func (streamError *StreamError) Unwrap() error {
	if streamError.Provider != nil {
		return streamError.Provider
	}
	return streamError.Cause
}

// ProviderUnavailable reports a connection that ended before the provider
// stated a refusal.
func (streamError *StreamError) ProviderUnavailable() bool {
	return streamError.Provider == nil || streamError.Provider.ProviderUnavailable()
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

// ProviderUnavailable reports a server-side failure rather than a refusal of
// this request.
func (providerError *ProviderError) ProviderUnavailable() bool {
	if providerError.StatusCode >= http.StatusInternalServerError && providerError.StatusCode <= 599 {
		return true
	}
	status, found := upstreamStatus(providerError)
	return found && status >= http.StatusInternalServerError && status <= 599
}

func upstreamStatus(providerError *ProviderError) (int, bool) {
	if !strings.EqualFold(providerError.Type, "invalid_request_error") ||
		!strings.EqualFold(providerError.Code, "upstream_failed") {
		return 0, false
	}
	message := strings.ToLower(strings.TrimSpace(providerError.Message))
	const prefix = "upstream call failed:"
	if !strings.HasPrefix(message, prefix) {
		return 0, false
	}
	metadata := strings.TrimSpace(strings.TrimPrefix(message, prefix))
	for _, boundary := range []string{"upstream_message=", "upstream message "} {
		if index := strings.Index(metadata, boundary); index >= 0 {
			metadata = metadata[:index]
		}
	}
	normalized := strings.NewReplacer("_", " ", "=", " ", ":", " ").Replace(metadata)
	fields := strings.Fields(normalized)
	for index := 0; index+2 < len(fields); index++ {
		if fields[index] != "upstream" || fields[index+1] != "status" {
			continue
		}
		status, err := strconv.Atoi(strings.Trim(fields[index+2], "(),.;"))
		if err == nil {
			return status, true
		}
	}
	return 0, false
}

// providerErrorFields are the fields the gateway states when it refuses a
// request, in the stream and in an HTTP body alike.
type providerErrorFields struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param"`
}

// stated reports whether the gateway named a reason. An object carrying none of
// these fields is not a refusal.
func (fields providerErrorFields) stated() bool {
	return fields.Message != "" || fields.Type != "" || fields.Code != ""
}

// streamErrorFrame is the wrapped shape the gateway writes into a stream. The
// SDK sometimes reports the wrapper and sometimes reports the inner object
// alone, so both shapes are read.
type streamErrorFrame struct {
	Error providerErrorFields `json:"error"`
}

// decodeStreamErrorFrame reads one error frame and reports whether it stated a
// refusal. Text that does not decode, or that decodes to an empty error, is not
// a refusal, so it reports false rather than an error.
func decodeStreamErrorFrame(encoded string) (providerErrorFields, bool) {
	var wrapped streamErrorFrame
	if err := json.Unmarshal([]byte(encoded), &wrapped); err == nil && wrapped.Error.stated() {
		return wrapped.Error, true
	}
	var bare providerErrorFields
	if err := json.Unmarshal([]byte(encoded), &bare); err == nil && bare.stated() {
		return bare, true
	}
	return providerErrorFields{Message: "", Type: "", Code: "", Param: ""}, false
}

// providerErrorFromStream reads the structured provider error out of a stream
// failure, so a refusal delivered mid-stream classifies exactly like the same
// refusal delivered as an HTTP status.
//
// The SDK reports the frame by embedding its raw JSON in the error text, so the
// object is recovered from the first brace onward. A failure carrying no such
// object is a dropped connection rather than a stated refusal, so it returns
// nil and the stream error reports the transport cause.
func providerErrorFromStream(err error) *ProviderError {
	text := err.Error()
	start := strings.Index(text, "{")
	if start < 0 {
		return nil
	}
	end := strings.LastIndex(text, "}")
	if end <= start {
		return nil
	}

	frame, ok := decodeStreamErrorFrame(text[start : end+1])
	if !ok {
		return nil
	}
	// The gateway states no HTTP status inside the frame, and it flattens every
	// upstream status onto 400 on the path that does carry one. Recording 400
	// keeps both transports reporting the same status for the same refusal.
	return &ProviderError{
		StatusCode: http.StatusBadRequest,
		Type:       frame.Type,
		Code:       frame.Code,
		Param:      frame.Param,
		Message:    frame.Message,
	}
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

// ProviderReason returns the provider's own sentence about what went wrong, so
// a caller can report the cause rather than the stage it happened in. It falls
// back to the code and type when the provider stated no message.
func (providerError *ProviderError) ProviderReason() string {
	for _, field := range []string{providerError.Message, providerError.Code, providerError.Type} {
		collapsed := strings.Join(strings.Fields(field), " ")
		if collapsed != "" {
			return collapsed
		}
	}
	return ""
}

// ProviderReason returns the refusal the stream frame stated. A dropped
// connection states none, so it reports the connection failure instead.
func (streamError *StreamError) ProviderReason() string {
	if streamError.Provider != nil {
		return streamError.Provider.ProviderReason()
	}
	return "the connection carrying the answer closed early"
}

// ProviderReason states that the model filled its answer budget, which is a
// property of the request size rather than a provider fault.
func (truncatedError *TruncatedError) ProviderReason() string {
	return "the model reached its completion token budget"
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

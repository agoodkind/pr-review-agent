package quota

import "errors"

// Limit applies a token ceiling to a selected accounting window.
type Limit struct {
	Limit      int64       `json:"limit"`
	TokenTypes []TokenType `json:"token_types,omitempty"`
	Window     Window      `json:"window"`
}

// Validate permits zero for reporting and requires a ceiling within the storage integer range.
func (limit Limit) Validate() error {
	if limit.Limit < 0 || limit.Limit > MaximumInteger {
		return errors.New("token limit must not be negative and must remain within the storage integer range")
	}
	seen := make(map[TokenType]bool, len(limit.TokenTypes))
	for _, tokenType := range limit.TokenTypes {
		if (tokenType != InputTokens && tokenType != OutputTokens) || seen[tokenType] {
			return errors.New("token types must contain input and/or output once")
		}
		seen[tokenType] = true
	}
	return limit.Window.Validate()
}

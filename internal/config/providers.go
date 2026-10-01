package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

// TokenType selects an API usage category for a provider's daily limit.
type TokenType string

const (
	// InputTokens includes cached input tokens.
	InputTokens TokenType = "input"
	// OutputTokens includes reasoning tokens.
	OutputTokens TokenType = "output"
)

// ProviderConfig contains one validated model endpoint and its credentials.
type ProviderConfig struct {
	ID                   string
	BaseURL              *url.URL
	Model                string
	APIKey               string
	DailyTokenLimit      int64
	DailyTokenTypes      []TokenType
	OmitMaxOutputTokens  bool
	OmitTextFormat       bool
	CFAccessClientID     string
	CFAccessClientSecret string
	// Disabled leaves this provider in the configuration and in the priority list.
	// The service sends it no requests. Its credentials are still required, so clearing the flag is enough to use it again.
	Disabled bool
}

type providerDefinition struct {
	ID                          string      `json:"id"`
	BaseURL                     string      `json:"base_url"`
	Model                       string      `json:"model"`
	APIKeyBinding               string      `json:"api_key_binding"`
	DailyTokenLimit             int64       `json:"daily_token_limit,omitempty"`
	DailyTokenTypes             []TokenType `json:"daily_token_types,omitempty"`
	OmitMaxOutputTokens         bool        `json:"omit_max_output_tokens,omitempty"`
	OmitTextFormat              bool        `json:"omit_text_format,omitempty"`
	CFAccessClientIDBinding     string      `json:"cf_access_client_id_binding,omitempty"`
	CFAccessClientSecretBinding string      `json:"cf_access_client_secret_binding,omitempty"`
	Disabled                    bool        `json:"disabled,omitempty"`
}

var providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func loadProviders(lookup LookupEnv) ([]ProviderConfig, error) {
	raw, _ := lookup("PROVIDERS")
	definitions, err := unmarshalProviderDefinitions(raw)
	if err != nil || len(definitions) == 0 {
		return nil, errors.New("PROVIDERS must be a nonempty array of providers")
	}
	priorityRaw, ok := lookup("PROVIDER_PRIORITY")
	if !ok {
		return nil, errors.New("PROVIDER_PRIORITY is required")
	}
	priority, err := unmarshalProviderPriority(priorityRaw)
	if err != nil || len(priority) != len(definitions) {
		return nil, errors.New("PROVIDER_PRIORITY must list each provider exactly once")
	}
	configured := make(map[string]ProviderConfig, len(definitions))
	for _, definition := range definitions {
		if !providerIDPattern.MatchString(definition.ID) || strings.TrimSpace(definition.Model) == "" || strings.TrimSpace(definition.APIKeyBinding) == "" {
			return nil, errors.New("each provider requires an id, model, and api_key_binding")
		}
		if definition.DailyTokenLimit < 0 {
			return nil, fmt.Errorf("provider %q daily_token_limit must not be negative", definition.ID)
		}
		if err := validateTokenTypes(definition.ID, definition.DailyTokenTypes); err != nil {
			return nil, err
		}
		if _, exists := configured[definition.ID]; exists {
			return nil, fmt.Errorf("duplicate provider %q", definition.ID)
		}
		baseURL, err := parseRequiredHTTPSURL("provider base_url", definition.BaseURL)
		if err != nil {
			return nil, err
		}
		prefix := "PROVIDER_" + strings.ToUpper(definition.ID)
		apiKey, ok := loadRequiredText(lookup, prefix+"_API_KEY")
		if !ok {
			return nil, fmt.Errorf("%s_API_KEY is required", prefix)
		}
		clientID, hasClientID := loadRequiredText(lookup, prefix+"_CF_ACCESS_CLIENT_ID")
		clientSecret, hasClientSecret := loadRequiredText(lookup, prefix+"_CF_ACCESS_CLIENT_SECRET")
		configuredID := definition.CFAccessClientIDBinding != ""
		configuredSecret := definition.CFAccessClientSecretBinding != ""
		if configuredID != configuredSecret || hasClientID != configuredID || hasClientSecret != configuredSecret {
			return nil, fmt.Errorf("%s requires both Cloudflare Access credentials", prefix)
		}
		configured[definition.ID] = ProviderConfig{
			ID:                   definition.ID,
			BaseURL:              baseURL,
			Model:                definition.Model,
			APIKey:               apiKey,
			DailyTokenLimit:      definition.DailyTokenLimit,
			DailyTokenTypes:      definition.DailyTokenTypes,
			OmitMaxOutputTokens:  definition.OmitMaxOutputTokens,
			OmitTextFormat:       definition.OmitTextFormat,
			CFAccessClientID:     clientID,
			CFAccessClientSecret: clientSecret, // gitleaks:allow
			Disabled:             definition.Disabled,
		}
	}
	providers := make([]ProviderConfig, 0, len(priority))
	seen := make(map[string]bool, len(priority))
	enabled := 0
	for _, id := range priority {
		provider, exists := configured[id]
		if !exists || seen[id] {
			return nil, errors.New("PROVIDER_PRIORITY must list each provider exactly once")
		}
		seen[id] = true
		if !provider.Disabled {
			enabled++
		}
		providers = append(providers, provider)
	}
	if enabled == 0 {
		return nil, errors.New("at least one provider must be enabled")
	}
	return providers, nil
}

func validateTokenTypes(providerID string, tokenTypes []TokenType) error {
	seen := make(map[TokenType]bool, len(tokenTypes))
	for _, tokenType := range tokenTypes {
		if tokenType != InputTokens && tokenType != OutputTokens || seen[tokenType] {
			return fmt.Errorf("provider %q daily_token_types must contain input and/or output once", providerID)
		}
		seen[tokenType] = true
	}
	return nil
}

func unmarshalProviderDefinitions(raw string) ([]providerDefinition, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var definitions []providerDefinition
	if err := decoder.Decode(&definitions); err != nil {
		return nil, errors.New("invalid provider definitions JSON")
	}
	if err := rejectTrailingProviderJSON(decoder); err != nil {
		return nil, err
	}
	return definitions, nil
}

func unmarshalProviderPriority(raw string) ([]string, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	var priority []string
	if err := decoder.Decode(&priority); err != nil {
		return nil, errors.New("invalid provider priority JSON")
	}
	if err := rejectTrailingProviderJSON(decoder); err != nil {
		return nil, err
	}
	return priority, nil
}

func rejectTrailingProviderJSON(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

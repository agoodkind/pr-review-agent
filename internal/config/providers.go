package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"regexp"
	"strings"

	"goodkind.io/pr-review-agent/internal/quota"
)

// TokenType selects an API usage category for a provider's token limit.
type TokenType = quota.TokenType

// AutoRouterCostTier selects OpenRouter's auto routing price band.
type AutoRouterCostTier string

// ProviderAPI selects the request and response protocol for a provider.
type ProviderAPI string

// ReasoningEffort selects an analysis level supported by the configured model.
type ReasoningEffort string

const (
	// ReasoningNone disables analysis and is rejected by native Gemini configuration.
	ReasoningNone ReasoningEffort = "none"
	// ReasoningMinimal requests the least intensive supported analysis level.
	ReasoningMinimal ReasoningEffort = "minimal"
	// ReasoningLow favors shorter reasoning over additional analysis.
	ReasoningLow ReasoningEffort = "low"
	// ReasoningMedium requests more analysis than low effort.
	ReasoningMedium ReasoningEffort = "medium"
	// ReasoningHigh requests additional analysis before the answer.
	ReasoningHigh ReasoningEffort = "high"
	// ReasoningXHigh is rejected by native Gemini configuration.
	ReasoningXHigh ReasoningEffort = "xhigh"
	// DefaultReasoningEffort applies when a provider omits reasoning_effort.
	DefaultReasoningEffort = ReasoningHigh
)

const (
	// ResponsesAPI uses the Responses API.
	ResponsesAPI ProviderAPI = "responses"
	// ChatCompletionsAPI uses the Chat Completions API.
	ChatCompletionsAPI ProviderAPI = "chat_completions"
	// GeminiAPI uses Google's native GenerateContent API.
	GeminiAPI ProviderAPI = "gemini"
)

const (
	// InputTokens includes cached input tokens.
	InputTokens TokenType = "input"
	// OutputTokens includes reasoning tokens.
	OutputTokens TokenType = "output"
)

const (
	// AutoRouterModel selects OpenRouter's automatic model router.
	AutoRouterModel = "openrouter/auto"
	// FreeRouterModel selects OpenRouter's free model router.
	FreeRouterModel = "openrouter/free"
	// AutoRouterCostLow selects the lowest price band.
	AutoRouterCostLow AutoRouterCostTier = "low"
	// AutoRouterCostMedium selects the medium price band.
	AutoRouterCostMedium AutoRouterCostTier = "medium"
	// AutoRouterCostHigh selects the high price band.
	AutoRouterCostHigh AutoRouterCostTier = "high"
	// AutoRouterCostXHigh selects the extra high price band.
	AutoRouterCostXHigh AutoRouterCostTier = "xhigh"
	// AutoRouterCostMax selects the maximum price band.
	AutoRouterCostMax AutoRouterCostTier = "max"
)

// ProviderConfig contains one validated model endpoint and its credentials.
type ProviderConfig struct {
	ID                   string
	BaseURL              *url.URL
	Model                string
	ReasoningEffort      ReasoningEffort
	APIKey               string
	DailyTokenLimit      int64
	DailyTokenTypes      []TokenType
	TokenLimit           int64
	TokenTypes           []TokenType
	TokenWindow          quota.Window
	MaxOutputTokens      int64
	OmitMaxOutputTokens  bool
	OmitTextFormat       bool
	AutoRouterCostTier   AutoRouterCostTier
	API                  ProviderAPI
	CFAccessClientID     string
	CFAccessClientSecret string
	// Disabled keeps this provider ready for later use without sending it requests.
	Disabled bool
}

type providerDefinition struct {
	ID                          string             `json:"id"`
	BaseURL                     string             `json:"base_url"`
	Model                       string             `json:"model"`
	ReasoningEffort             ReasoningEffort    `json:"reasoning_effort,omitempty"`
	APIKeyBinding               string             `json:"api_key_binding"`
	DailyTokenLimit             *int64             `json:"daily_token_limit,omitempty"`
	DailyTokenTypes             []TokenType        `json:"daily_token_types,omitempty"`
	TokenLimit                  *int64             `json:"token_limit,omitempty"`
	TokenTypes                  []TokenType        `json:"token_types,omitempty"`
	TokenWindow                 *quota.Window      `json:"token_window,omitempty"`
	MaxOutputTokens             int64              `json:"max_output_tokens,omitempty"`
	OmitMaxOutputTokens         bool               `json:"omit_max_output_tokens,omitempty"`
	OmitTextFormat              bool               `json:"omit_text_format,omitempty"`
	AutoRouterCostTier          AutoRouterCostTier `json:"auto_router_cost_tier,omitempty"`
	API                         ProviderAPI        `json:"api_kind,omitempty"`
	CFAccessClientIDBinding     string             `json:"cf_access_client_id_binding,omitempty"`
	CFAccessClientSecretBinding string             `json:"cf_access_client_secret_binding,omitempty"`
	Disabled                    bool               `json:"disabled,omitempty"`
}

var providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// LoadProviders requires normalized PROVIDER_<ID> credential variables.
func LoadProviders(lookup LookupEnv) ([]ProviderConfig, error) {
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
		if definition.dailyLimit() < 0 {
			return nil, fmt.Errorf("provider %q daily_token_limit must not be negative", definition.ID)
		}
		if err := validateProviderLimits(definition); err != nil {
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
			ReasoningEffort:      definition.ReasoningEffort,
			APIKey:               apiKey,
			DailyTokenLimit:      definition.dailyLimit(),
			DailyTokenTypes:      definition.DailyTokenTypes,
			TokenLimit:           definition.tokenLimit(),
			TokenTypes:           definition.TokenTypes,
			TokenWindow:          definition.tokenWindow(),
			MaxOutputTokens:      definition.MaxOutputTokens,
			OmitMaxOutputTokens:  definition.OmitMaxOutputTokens,
			OmitTextFormat:       definition.OmitTextFormat,
			AutoRouterCostTier:   definition.AutoRouterCostTier,
			API:                  definition.API,
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

func (definition providerDefinition) dailyLimit() int64 {
	if definition.DailyTokenLimit == nil {
		return 0
	}
	return *definition.DailyTokenLimit
}

func (definition providerDefinition) tokenLimit() int64 {
	if definition.TokenLimit == nil {
		return 0
	}
	return *definition.TokenLimit
}

func (definition providerDefinition) tokenWindow() quota.Window {
	if definition.TokenWindow == nil {
		var window quota.Window
		return window
	}
	return *definition.TokenWindow
}

func validateProviderQuota(definition providerDefinition) error {
	if definition.dailyLimit() > quota.MaximumInteger {
		return fmt.Errorf("provider %q daily_token_limit exceeds the storage integer range", definition.ID)
	}
	if definition.TokenLimit != nil {
		if definition.DailyTokenLimit != nil || definition.DailyTokenTypes != nil {
			return fmt.Errorf("provider %q cannot mix daily and generic token limits", definition.ID)
		}
		if *definition.TokenLimit < 0 || *definition.TokenLimit > quota.MaximumInteger {
			return fmt.Errorf("provider %q token_limit is outside the storage integer range", definition.ID)
		}
		if *definition.TokenLimit > 0 && definition.TokenWindow == nil {
			return fmt.Errorf("provider %q token_limit requires token_window", definition.ID)
		}
	} else if definition.TokenWindow != nil || len(definition.TokenTypes) != 0 {
		return fmt.Errorf("provider %q token_window and token_types require token_limit", definition.ID)
	}
	if definition.TokenWindow != nil {
		if err := definition.TokenWindow.Validate(); err != nil {
			slog.Warn("invalid provider quota window", slog.String("provider_id", definition.ID), slog.String("err", err.Error()))
			return fmt.Errorf("provider %q token_window: %w", definition.ID, err)
		}
	}
	return nil
}

func validateProviderLimits(definition providerDefinition) error {
	switch definition.ReasoningEffort {
	case ReasoningNone, ReasoningMinimal, ReasoningLow, ReasoningMedium, ReasoningHigh, ReasoningXHigh:
	default:
		return fmt.Errorf("provider %q reasoning_effort must be none, minimal, low, medium, high, or xhigh", definition.ID)
	}
	if err := validateProviderQuota(definition); err != nil {
		return err
	}
	if definition.API != ResponsesAPI && definition.API != ChatCompletionsAPI && definition.API != GeminiAPI {
		return fmt.Errorf("provider %q api_kind must be responses, chat_completions, or gemini", definition.ID)
	}
	if definition.API == GeminiAPI && (definition.ReasoningEffort == ReasoningNone || definition.ReasoningEffort == ReasoningXHigh) {
		return fmt.Errorf("provider %q Gemini reasoning_effort must be minimal, low, medium, or high", definition.ID)
	}
	if definition.API != ResponsesAPI && definition.AutoRouterCostTier != "" {
		return fmt.Errorf("provider %q auto_router_cost_tier requires the Responses API", definition.ID)
	}
	if definition.AutoRouterCostTier != "" {
		if definition.Model != AutoRouterModel {
			return fmt.Errorf("provider %q auto_router_cost_tier requires openrouter/auto", definition.ID)
		}
		switch definition.AutoRouterCostTier {
		case AutoRouterCostLow, AutoRouterCostMedium, AutoRouterCostHigh, AutoRouterCostXHigh, AutoRouterCostMax:
		default:
			return fmt.Errorf("provider %q auto_router_cost_tier must be low, medium, high, xhigh, or max", definition.ID)
		}
	}
	if definition.MaxOutputTokens < 0 || definition.MaxOutputTokens > math.MaxInt32 {
		return fmt.Errorf("provider %q max_output_tokens must be positive when set and fit a 32-bit signed integer", definition.ID)
	}
	if definition.MaxOutputTokens != 0 && definition.OmitMaxOutputTokens {
		return fmt.Errorf("provider %q cannot set both max_output_tokens and omit_max_output_tokens", definition.ID)
	}
	tokenTypes := definition.DailyTokenTypes
	if definition.TokenLimit != nil {
		tokenTypes = definition.TokenTypes
	}
	seen := make(map[TokenType]bool, len(tokenTypes))
	for _, tokenType := range tokenTypes {
		if tokenType != InputTokens && tokenType != OutputTokens || seen[tokenType] {
			return fmt.Errorf("provider %q token types must contain input and/or output once", definition.ID)
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
	for index := range definitions {
		if definitions[index].ReasoningEffort == "" {
			definitions[index].ReasoningEffort = DefaultReasoningEffort
		}
		if definitions[index].API == "" {
			definitions[index].API = ResponsesAPI
		}
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

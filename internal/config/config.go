// Package config loads public runtime settings from a file and credentials from the environment.
package config

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/reviewrules"
)

const (
	// FallbackOnUsageExceeded is the only supported fallback trigger. It sends
	// the request to the fallback provider when the primary reports that it has
	// no remaining usage.
	FallbackOnUsageExceeded = "usage_exceeded"
)

// FailureClass separates failures that operators may present differently on GitHub.
type FailureClass string

const (
	// FailureUsageExceeded lets operators treat exhausted provider accounts as nonblocking.
	FailureUsageExceeded FailureClass = "usage_exceeded"
	// FailureRateLimited separates provider throttling from exhausted usage.
	FailureRateLimited FailureClass = "rate_limited"
	// FailureDailyBudget keeps local budget denials independent from provider usage policy.
	FailureDailyBudget FailureClass = "daily_budget"
	// FailureUnavailable keeps connectivity failures independent from usage policy.
	FailureUnavailable FailureClass = "provider_unavailable"
	// FailureDeadline keeps timeouts independent from provider availability policy.
	FailureDeadline FailureClass = "deadline"
	// FailurePanic lets operators preserve a blocking result for recovered internal faults.
	FailurePanic FailureClass = "panic"
	// FailureOther keeps unrecognized failures blocking unless operators explicitly allow them.
	FailureOther FailureClass = "other"
)

// FailureAppearance controls whether one failure class blocks the GitHub check.
type FailureAppearance string

const (
	// FailureAppearanceFail preserves the blocking conclusion for the run.
	FailureAppearanceFail FailureAppearance = "fail"
	// FailureAppearancePass allows the check to conclude with success.
	FailureAppearancePass FailureAppearance = "pass"
)

// FailureAppearances lets operators choose the check result for each failure class.
type FailureAppearances map[FailureClass]FailureAppearance

// Passes reports whether class is allowed to conclude the check with success.
func (appearances FailureAppearances) Passes(class FailureClass) bool {
	return appearances[class] == FailureAppearancePass
}

const (
	// ReviewCheckName is the GitHub check run name for review lifecycle.
	ReviewCheckName = "PR-Agent Review"
	// QueueCapacity is the maximum number of queued review jobs.
	QueueCapacity = 100
	// DeliveryCacheCapacity is the maximum number of cached delivery ids.
	DeliveryCacheCapacity = 10000
	// DeliveryCacheTTL is how long a delivery claim remains reserved.
	DeliveryCacheTTL = 24 * time.Hour
	// MaximumWebhookBytes is the maximum accepted webhook body size.
	MaximumWebhookBytes = 2 * 1024 * 1024
	// MaximumPromptBytes is the maximum model prompt size for one batch.
	MaximumPromptBytes = 80000
	// MaximumOutputTokens is the maximum model completion size.
	MaximumOutputTokens = 8000
	// MaximumChunkConcurrency is how many chunks of one delta are reviewed at
	// once. Each call still carries its own timeout, so nothing here is a clock
	// spanning two calls; what this bounds is wall clock. A sixty chunk delta
	// reviewed strictly one at a time would run for hours at the measured call
	// durations, long past any reader's patience or a container's lifetime.
	// Four keeps the provider's concurrent load modest while cutting that to
	// fifteen waves.
	MaximumChunkConcurrency = 4
	// GitHubAPIVersion is the GitHub REST API version sent on every request.
	GitHubAPIVersion = "2022-11-28"
	// DefaultReviewMaxFiles is the file count budget past which a delta is
	// declined before any model call.
	DefaultReviewMaxFiles = 100
	// DefaultReviewMaxChunks is the diff chunk budget past which a delta is
	// declined before any model call.
	DefaultReviewMaxChunks = 60
	// DefaultReviewChunkTimeout is the timeout for one model call. The
	// measured worst completed call was 2m19s.
	DefaultReviewChunkTimeout = 5 * time.Minute
)

// LookupEnv reads one environment variable.
type LookupEnv func(string) (string, bool)

const runtimeConfigPath = "/runtime.json"

var runtimeConfigKeys = map[string]struct{}{
	"CLYDE_BASE_URL":             {},
	"CONTAINER_SLEEP_AFTER":      {},
	"FALLBACK_BASE_URL":          {},
	"FALLBACK_MODEL":             {},
	"FALLBACK_ON":                {},
	"PROVIDERS":                  {},
	"PROVIDER_PRIORITY":          {},
	"PROVIDER_FAILURE_POLICY":    {},
	"PROVIDER_BUDGET_URL":        {},
	"SERVICE_FAILURE_APPEARANCE": {},
	"GITHUB_APP_ID":              {},
	"GITHUB_BOT_LOGIN":           {},
	"LOG_FORWARD_URL":            {},
	"PORT":                       {},
	"REVIEW_CHUNK_TIMEOUT":       {},
	"REVIEW_CHUNK_CONCURRENCY":   {},
	"REVIEW_MAX_CHUNKS":          {},
	"REVIEW_MAX_FILES":           {},
	"REVIEW_MAX_PROMPT_BYTES":    {},
	"REVIEW_MIN_IMPORTANCE":      {},
	"REVIEW_RULE_IMPORTANCE":     {},
	"REVIEW_RULES_FILE":          {},
	"REVIEW_PROMPTS_FILE":        {},
	"REVIEW_MODEL":               {},
	"REVIEW_MODEL_PRICING":       {},
	"REVIEW_WORKERS":             {},
}

// ModelPricing holds estimated US dollar rates per million tokens.
type ModelPricing struct {
	InputPerMillionTokens       float64 `json:"input_per_million_tokens"`
	CachedInputPerMillionTokens float64 `json:"cached_input_per_million_tokens"`
	OutputPerMillionTokens      float64 `json:"output_per_million_tokens"`
}

// Config holds validated service configuration.
type Config struct {
	Port                  string
	ReviewWorkers         int
	ReviewModel           string
	ReviewModelPricing    map[string]ModelPricing
	Providers             []ProviderConfig
	ProviderFailurePolicy ProviderFailurePolicy
	ProviderBudgetURL     *url.URL
	// ReviewMaxFiles and ReviewMaxChunks bound one run. Admission, not a
	// timer, is what keeps a review finishable, so these are the only limits
	// on how much work one invocation accepts.
	ReviewMaxFiles         int
	ReviewMaxChunks        int
	ReviewChunkConcurrency int
	ReviewMaxPromptBytes   int
	// ReviewChunkTimeout is the only clock in a review. It bounds one model
	// call, and no clock spans two of them.
	ReviewChunkTimeout           time.Duration
	MinimumImportance            int
	RuleImportance               reviewrules.Importance
	ReviewPolicy                 reviewrules.Policy
	GitHubAppID                  int64
	GitHubPrivateKey             *rsa.PrivateKey
	GitHubWebhookSecret          []byte
	GitHubBotLogin               string
	GitHubAPIBaseURL             *url.URL
	GitHubGraphQLURL             *url.URL
	ClydeBaseURL                 *url.URL
	ClydeAPIKey                  string
	CFAccessClientID             string
	CFAccessClientSecret         string
	FallbackBaseURL              *url.URL
	FallbackModel                string
	FallbackAPIKey               string
	FallbackCFAccessClientID     string
	FallbackCFAccessClientSecret string
	FallbackOnUsageExceeded      bool
	// ServiceFailureAppearance allows expected service failures to leave the required check green.
	ServiceFailureAppearance FailureAppearances
	// LogForwardURL is where the service ships its own logs so a person can
	// read them. Container stdout reaches no log sink, so without this the
	// service is invisible in production. It is optional, and an empty value
	// leaves the service logging only to stdout.
	LogForwardURL *url.URL
}

// HasFallback reports whether a fallback model provider is configured.
func (cfg Config) HasFallback() bool {
	return cfg.FallbackBaseURL != nil && cfg.FallbackModel != "" && cfg.FallbackAPIKey != ""
}

// ChunkConcurrency supplies the default when a directly constructed Config omits the limit.
func (cfg Config) ChunkConcurrency() int {
	if cfg.ReviewChunkConcurrency == 0 {
		return MaximumChunkConcurrency
	}
	return cfg.ReviewChunkConcurrency
}

// PromptBytes supplies the default when a directly constructed Config omits the limit.
func (cfg Config) PromptBytes() int {
	if cfg.ReviewMaxPromptBytes == 0 {
		return MaximumPromptBytes
	}
	return cfg.ReviewMaxPromptBytes
}

// LoadReviewLimits validates optional runtime limits shared by service and diagnostic callers.
func LoadReviewLimits(lookup LookupEnv, cfg *Config) error {
	var err error
	cfg.ReviewChunkConcurrency, err = loadPositiveSetting(lookup, "REVIEW_CHUNK_CONCURRENCY")
	if err != nil {
		return err
	}
	cfg.ReviewMaxPromptBytes, err = loadPositiveSetting(lookup, "REVIEW_MAX_PROMPT_BYTES")
	return err
}

func loadPositiveSetting(lookup LookupEnv, name string) (int, error) {
	value, configured := lookup(name)
	if !configured {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

// PricingForModel returns the exact or longest prefix pricing match for a model.
func (cfg Config) PricingForModel(model string) (ModelPricing, bool) {
	return FindModelPricing(cfg.ReviewModelPricing, model)
}

// FindModelPricing returns the exact or longest dashed prefix match for a model.
func FindModelPricing(pricingByModel map[string]ModelPricing, model string) (ModelPricing, bool) {
	if pricing, ok := pricingByModel[model]; ok {
		return pricing, true
	}
	matchedPrefix := ""
	var matchedPricing ModelPricing
	for prefix, pricing := range pricingByModel {
		if !strings.HasPrefix(model, prefix+"-") || len(prefix) <= len(matchedPrefix) {
			continue
		}
		matchedPrefix = prefix
		matchedPricing = pricing
	}
	return matchedPricing, matchedPrefix != ""
}

// FromEnvironment loads public settings from the deployed file and secrets from the environment.
func FromEnvironment() (Config, error) {
	data, err := os.ReadFile(runtimeConfigPath)
	if err != nil {
		slog.Error("read runtime configuration", "error", err)
		return Config{}, fmt.Errorf("read runtime configuration: %w", err)
	}
	return LoadRuntime(data, os.LookupEnv)
}

// LoadRuntime parses public JSON settings and reads credentials from lookup.
func LoadRuntime(data []byte, lookup LookupEnv) (Config, error) {
	var rawValues map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawValues); err != nil || rawValues == nil {
		return Config{}, errors.New("runtime configuration must be a JSON object")
	}
	values := make(map[string]string, len(rawValues))
	for name, raw := range rawValues {
		if _, ok := runtimeConfigKeys[name]; !ok {
			return Config{}, fmt.Errorf("unknown runtime configuration key %q", name)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return Config{}, fmt.Errorf("runtime configuration key %q must not be null", name)
		}
		if name == "REVIEW_MODEL_PRICING" || name == "PROVIDERS" || name == "PROVIDER_PRIORITY" || name == "SERVICE_FAILURE_APPEARANCE" || name == "REVIEW_RULE_IMPORTANCE" {
			values[name] = string(raw)
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return Config{}, fmt.Errorf("runtime configuration key %q must be a string", name)
		}
		values[name] = value
	}
	return Load(func(name string) (string, bool) {
		if _, ok := runtimeConfigKeys[name]; ok {
			value, found := values[name]
			return value, found
		}
		return lookup(name)
	})
}

// Load validates configuration loaded through lookup.
func Load(lookup LookupEnv) (Config, error) {
	cfg, missing := loadBase(lookup)
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if err := LoadReviewLimits(lookup, &cfg); err != nil {
		return Config{}, err
	}
	policy, err := LoadReviewPolicy(lookup)
	if err != nil {
		return Config{}, err
	}
	cfg.ReviewPolicy = policy
	pricing, err := loadModelPricing(lookup)
	if err != nil {
		return Config{}, err
	}
	cfg.ReviewModelPricing = pricing
	ruleImportance, err := loadRuleImportance(lookup, policy.Catalog())
	if err != nil {
		return Config{}, err
	}
	cfg.RuleImportance = ruleImportance
	appearances, err := loadFailureAppearances(lookup)
	if err != nil {
		return Config{}, err
	}
	cfg.ServiceFailureAppearance = appearances
	failurePolicy, err := LoadProviderFailurePolicy(lookup)
	if err != nil {
		return Config{}, err
	}
	cfg.ProviderFailurePolicy = failurePolicy
	if _, configured := lookup("PROVIDERS"); configured {
		providers, err := LoadProviders(lookup)
		if err != nil {
			return Config{}, err
		}
		cfg.Providers = providers
		for _, provider := range providers {
			if provider.Disabled || !provider.HasTokenLimit() {
				continue
			}
			budgetURL, ok := loadRequiredText(lookup, "PROVIDER_BUDGET_URL")
			if !ok {
				return Config{}, errors.New("PROVIDER_BUDGET_URL is required for capped providers")
			}
			cfg.ProviderBudgetURL, err = parseRequiredHTTPSURL("PROVIDER_BUDGET_URL", budgetURL)
			if err != nil {
				return Config{}, err
			}
			break
		}
	}

	apiBaseURL, err := url.Parse("https://api.github.com")
	if err != nil {
		return Config{}, errors.New("default GitHub API URL is invalid")
	}
	graphqlURL, err := url.Parse("https://api.github.com/graphql")
	if err != nil {
		return Config{}, errors.New("default GitHub GraphQL URL is invalid")
	}
	cfg.GitHubAPIBaseURL = apiBaseURL
	cfg.GitHubGraphQLURL = graphqlURL

	return cfg, nil
}

func loadRuleImportance(lookup LookupEnv, catalog reviewrules.Catalog) (reviewrules.Importance, error) {
	raw, configured := lookup("REVIEW_RULE_IMPORTANCE")
	var importance reviewrules.Importance
	if configured {
		decoder := json.NewDecoder(strings.NewReader(raw))
		if err := decoder.Decode(&importance); err != nil || importance == nil {
			return nil, errors.New("REVIEW_RULE_IMPORTANCE must be a JSON object of integer importance values")
		}
		var trailing json.RawMessage
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return nil, errors.New("REVIEW_RULE_IMPORTANCE must contain one JSON object")
		}
	}
	if err := catalog.ValidateImportance(importance); err != nil {
		slog.Error("validate review rule importance", "error", err)
		return nil, fmt.Errorf("validate REVIEW_RULE_IMPORTANCE: %w", err)
	}
	return importance, nil
}

func loadModelPricing(lookup LookupEnv) (map[string]ModelPricing, error) {
	value, ok := lookup("REVIEW_MODEL_PRICING")
	if !ok || strings.TrimSpace(value) == "" {
		return nil, nil
	}
	pricing := make(map[string]ModelPricing)
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pricing); err != nil {
		return nil, errors.New("REVIEW_MODEL_PRICING must be a JSON object of token rates")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("REVIEW_MODEL_PRICING must contain one JSON object")
	}
	for model, rates := range pricing {
		if strings.TrimSpace(model) == "" {
			return nil, errors.New("REVIEW_MODEL_PRICING model keys must not be empty")
		}
		if rates.InputPerMillionTokens < 0 ||
			rates.CachedInputPerMillionTokens < 0 ||
			rates.OutputPerMillionTokens < 0 {
			return nil, errors.New("REVIEW_MODEL_PRICING token rates must not be negative")
		}
	}
	return pricing, nil
}

func loadFailureAppearances(lookup LookupEnv) (FailureAppearances, error) {
	raw, ok := lookup("SERVICE_FAILURE_APPEARANCE")
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	var parsed map[FailureClass]FailureAppearance
	if err := decoder.Decode(&parsed); err != nil {
		return nil, errors.New("SERVICE_FAILURE_APPEARANCE must be a JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("SERVICE_FAILURE_APPEARANCE must contain one JSON object")
	}
	for class, appearance := range parsed {
		if _, known := knownFailureClasses[class]; !known {
			return nil, fmt.Errorf("SERVICE_FAILURE_APPEARANCE has unknown failure class %q", class)
		}
		if appearance != FailureAppearanceFail && appearance != FailureAppearancePass {
			return nil, fmt.Errorf("SERVICE_FAILURE_APPEARANCE appearance for %q must be %q or %q", class, FailureAppearanceFail, FailureAppearancePass)
		}
	}
	return parsed, nil
}

var knownFailureClasses = map[FailureClass]struct{}{
	FailureUsageExceeded: {},
	FailureRateLimited:   {},
	FailureDailyBudget:   {},
	FailureUnavailable:   {},
	FailureDeadline:      {},
	FailurePanic:         {},
	FailureOther:         {},
}

func loadBase(lookup LookupEnv) (Config, []string) {
	var cfg Config
	var missing []string

	cfg.Port = loadPort(lookup)
	reviewWorkers, ok := loadReviewWorkers(lookup)
	if !ok {
		missing = append(missing, "REVIEW_WORKERS")
	} else {
		cfg.ReviewWorkers = reviewWorkers
	}
	minimumImportance, ok := loadMinimumImportance(lookup)
	if !ok {
		missing = append(missing, "REVIEW_MIN_IMPORTANCE")
	} else {
		cfg.MinimumImportance = minimumImportance
	}
	if _, configured := lookup("PROVIDERS"); !configured {
		reviewModel, ok := loadRequiredText(lookup, "REVIEW_MODEL")
		if !ok {
			missing = append(missing, "REVIEW_MODEL")
		} else {
			cfg.ReviewModel = reviewModel
		}
	}
	reviewMaxFiles, ok := loadReviewMaxFiles(lookup)
	if !ok {
		missing = append(missing, "REVIEW_MAX_FILES")
	} else {
		cfg.ReviewMaxFiles = reviewMaxFiles
	}
	reviewMaxChunks, ok := loadReviewMaxChunks(lookup)
	if !ok {
		missing = append(missing, "REVIEW_MAX_CHUNKS")
	} else {
		cfg.ReviewMaxChunks = reviewMaxChunks
	}
	reviewChunkTimeout, ok := loadReviewChunkTimeout(lookup)
	if !ok {
		missing = append(missing, "REVIEW_CHUNK_TIMEOUT")
	} else {
		cfg.ReviewChunkTimeout = reviewChunkTimeout
	}
	missing = append(missing, loadGitHub(lookup, &cfg)...)
	if _, configured := lookup("PROVIDERS"); !configured {
		missing = append(missing, loadClyde(lookup, &cfg)...)
		missing = append(missing, loadFallback(lookup, &cfg)...)
	}
	cfg.LogForwardURL = loadLogForwardURL(lookup)

	return cfg, missing
}

// loadLogForwardURL reads the optional log destination. A malformed value is
// treated as absent rather than fatal, because losing log shipping must never
// stop the service from reviewing.
func loadLogForwardURL(lookup LookupEnv) *url.URL {
	value, ok := lookup("LOG_FORWARD_URL")
	if !ok || strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	return parsed
}

func loadRequiredText(lookup LookupEnv, name string) (string, bool) {
	value, ok := lookup(name)
	if !ok || strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

func loadReviewWorkers(lookup LookupEnv) (int, bool) {
	value, ok := lookup("REVIEW_WORKERS")
	if !ok || strings.TrimSpace(value) == "" {
		return 0, false
	}
	workers, err := strconv.Atoi(value)
	if err != nil || workers <= 0 {
		return 0, false
	}
	return workers, true
}

// loadReviewMaxFiles reads the file count admission budget. An unset value
// takes the default, but a present, non-positive value is rejected rather
// than silently falling back, so a typo cannot admit deltas the budget
// exists to decline.
func loadReviewMaxFiles(lookup LookupEnv) (int, bool) {
	value, ok := lookup("REVIEW_MAX_FILES")
	if !ok || strings.TrimSpace(value) == "" {
		return DefaultReviewMaxFiles, true
	}
	maxFiles, err := strconv.Atoi(value)
	if err != nil || maxFiles <= 0 {
		return 0, false
	}
	return maxFiles, true
}

// loadReviewMaxChunks reads the diff chunk admission budget, following the
// same default-unless-invalid pattern as loadReviewMaxFiles.
func loadReviewMaxChunks(lookup LookupEnv) (int, bool) {
	value, ok := lookup("REVIEW_MAX_CHUNKS")
	if !ok || strings.TrimSpace(value) == "" {
		return DefaultReviewMaxChunks, true
	}
	maxChunks, err := strconv.Atoi(value)
	if err != nil || maxChunks <= 0 {
		return 0, false
	}
	return maxChunks, true
}

// loadReviewChunkTimeout reads the timeout for one model call. An unset
// value takes the default; a present, non-positive duration is rejected.
func loadReviewChunkTimeout(lookup LookupEnv) (time.Duration, bool) {
	value, ok := lookup("REVIEW_CHUNK_TIMEOUT")
	if !ok || strings.TrimSpace(value) == "" {
		return DefaultReviewChunkTimeout, true
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return 0, false
	}
	return timeout, true
}

func loadMinimumImportance(lookup LookupEnv) (int, bool) {
	value, ok := lookup("REVIEW_MIN_IMPORTANCE")
	if !ok || strings.TrimSpace(value) == "" {
		return 0, false
	}
	importance, err := strconv.Atoi(value)
	if err != nil || importance < reviewrules.MinimumImportance || importance > reviewrules.MaximumImportance {
		return 0, false
	}
	return importance, true
}

func loadPort(lookup LookupEnv) string {
	port, ok := lookup("PORT")
	if !ok || strings.TrimSpace(port) == "" {
		return "3000"
	}
	return port
}

func loadGitHub(lookup LookupEnv, cfg *Config) []string {
	var missing []string

	appIDText, ok := lookup("GITHUB_APP_ID")
	if !ok || strings.TrimSpace(appIDText) == "" {
		missing = append(missing, "GITHUB_APP_ID")
	} else {
		appID, err := strconv.ParseInt(appIDText, 10, 64)
		if err != nil || appID <= 0 {
			missing = append(missing, "GITHUB_APP_ID")
		} else {
			cfg.GitHubAppID = appID
		}
	}

	privateKeyText, ok := lookup("GITHUB_PRIVATE_KEY")
	if !ok || strings.TrimSpace(privateKeyText) == "" {
		missing = append(missing, "GITHUB_PRIVATE_KEY")
	} else {
		privateKey, err := parseRSAPrivateKey(privateKeyText)
		if err != nil {
			missing = append(missing, "GITHUB_PRIVATE_KEY")
		} else {
			rsaKey := privateKey
			cfg.GitHubPrivateKey = rsaKey // gitleaks:allow
		}
	}

	webhookValue, ok := lookup("GITHUB_WEBHOOK_SECRET")
	if !ok || strings.TrimSpace(webhookValue) == "" {
		missing = append(missing, "GITHUB_WEBHOOK_SECRET")
	} else {
		cfg.GitHubWebhookSecret = []byte(webhookValue) // gitleaks:allow
	}

	botLogin, ok := lookup("GITHUB_BOT_LOGIN")
	if !ok || strings.TrimSpace(botLogin) == "" {
		missing = append(missing, "GITHUB_BOT_LOGIN")
	} else {
		cfg.GitHubBotLogin = botLogin
	}

	return missing
}

func loadClyde(lookup LookupEnv, cfg *Config) []string {
	var missing []string

	clydeBaseURL, ok := lookup("CLYDE_BASE_URL")
	if !ok || strings.TrimSpace(clydeBaseURL) == "" {
		missing = append(missing, "CLYDE_BASE_URL")
	} else {
		parsed, err := parseRequiredHTTPSURL("CLYDE_BASE_URL", clydeBaseURL)
		if err != nil {
			missing = append(missing, "CLYDE_BASE_URL")
		} else {
			cfg.ClydeBaseURL = parsed
		}
	}

	clydeAPIKey, ok := lookup("CLYDE_API_KEY")
	if !ok || strings.TrimSpace(clydeAPIKey) == "" {
		missing = append(missing, "CLYDE_API_KEY")
	} else {
		cfg.ClydeAPIKey = clydeAPIKey
	}

	cfClientID, hasClientID := loadRequiredText(lookup, "CF_ACCESS_CLIENT_ID")
	cfAccessValue, hasClientSecret := loadRequiredText(lookup, "CF_ACCESS_CLIENT_SECRET")
	if hasClientID != hasClientSecret {
		if !hasClientID {
			missing = append(missing, "CF_ACCESS_CLIENT_ID")
		}
		if !hasClientSecret {
			missing = append(missing, "CF_ACCESS_CLIENT_SECRET")
		}
	}
	if hasClientID && hasClientSecret {
		cfg.CFAccessClientID = cfClientID
		cfg.CFAccessClientSecret = cfAccessValue // gitleaks:allow
	}

	return missing
}

// fallbackCoreNames are the fallback variables that stand or fall together.
var fallbackCoreNames = []string{
	"FALLBACK_BASE_URL",
	"FALLBACK_MODEL",
	"FALLBACK_API_KEY",
}

// loadFallback reads the optional fallback model provider. Leaving every
// fallback variable unset keeps the service on one provider. Setting any core
// variable requires all of them, so a half-configured fallback fails at startup
// instead of at the moment the primary provider refuses a review.
func loadFallback(lookup LookupEnv, cfg *Config) []string {
	present := 0
	for _, name := range fallbackCoreNames {
		if _, ok := loadRequiredText(lookup, name); ok {
			present++
		}
	}
	if present == 0 {
		return nil
	}

	var missing []string
	baseURL, ok := loadRequiredText(lookup, "FALLBACK_BASE_URL")
	if !ok {
		missing = append(missing, "FALLBACK_BASE_URL")
	} else {
		parsed, err := parseRequiredHTTPSURL("FALLBACK_BASE_URL", baseURL)
		if err != nil {
			missing = append(missing, "FALLBACK_BASE_URL")
		} else {
			cfg.FallbackBaseURL = parsed
		}
	}

	model, ok := loadRequiredText(lookup, "FALLBACK_MODEL")
	if !ok {
		missing = append(missing, "FALLBACK_MODEL")
	} else {
		cfg.FallbackModel = model
	}

	apiKey, ok := loadRequiredText(lookup, "FALLBACK_API_KEY")
	if !ok {
		missing = append(missing, "FALLBACK_API_KEY")
	} else {
		cfg.FallbackAPIKey = apiKey // gitleaks:allow
	}

	missing = append(missing, loadFallbackAccess(lookup, cfg)...)
	if !loadFallbackTrigger(lookup, cfg) {
		missing = append(missing, "FALLBACK_ON")
	}
	return missing
}

// loadFallbackAccess reads the optional Cloudflare Access pair. A public
// endpoint needs no Access headers, so an unset pair is valid, but one value
// without the other would send an incomplete credential.
func loadFallbackAccess(lookup LookupEnv, cfg *Config) []string {
	clientID, hasClientID := loadRequiredText(lookup, "FALLBACK_CF_ACCESS_CLIENT_ID")
	clientSecret, hasClientSecret := loadRequiredText(lookup, "FALLBACK_CF_ACCESS_CLIENT_SECRET")
	if !hasClientID && !hasClientSecret {
		return nil
	}
	var missing []string
	if !hasClientID {
		missing = append(missing, "FALLBACK_CF_ACCESS_CLIENT_ID")
	}
	if !hasClientSecret {
		missing = append(missing, "FALLBACK_CF_ACCESS_CLIENT_SECRET")
	}
	if len(missing) > 0 {
		return missing
	}
	cfg.FallbackCFAccessClientID = clientID
	cfg.FallbackCFAccessClientSecret = clientSecret // gitleaks:allow
	return nil
}

// loadFallbackTrigger reads the declared fallback condition. An unset value
// means exhausted usage. An unrecognized value fails, so a typo cannot disable
// the fallback silently.
func loadFallbackTrigger(lookup LookupEnv, cfg *Config) bool {
	value, ok := loadRequiredText(lookup, "FALLBACK_ON")
	if !ok {
		cfg.FallbackOnUsageExceeded = true
		return true
	}
	if strings.TrimSpace(value) != FallbackOnUsageExceeded {
		return false
	}
	cfg.FallbackOnUsageExceeded = true
	return true
}

func parseRequiredHTTPSURL(name string, value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid URL", name)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("%s must be an absolute URL", name)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("%s must use HTTPS", name)
	}
	return parsed, nil
}

func parseRSAPrivateKey(value string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		return nil, errors.New("invalid PEM data")
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("malformed RSA private key")
	}
	rsaKey, ok := parsedKey.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rsaKey, nil
}

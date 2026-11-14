package opsruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

var (
	errInvalidRuntime    = errors.New("runtime configuration is invalid")
	errInvalidImportance = errors.New("runtime REVIEW_MIN_IMPORTANCE must be an integer in 1..10")
	errInvalidTimeout    = errors.New("runtime REVIEW_CHUNK_TIMEOUT must be a positive duration")
	errMissingSigningKey = errors.New(
		"capped provider probe requires --signing-key-file; configured limits are not bypassed",
	)
	errInvalidBudgetURL  = errors.New("capped provider requires an HTTPS PROVIDER_BUDGET_URL")
	errInvalidProvider   = errors.New("runtime provider definition is invalid")
	errDuplicateProvider = errors.New("runtime provider selection is duplicated")
	errUnknownProvider   = errors.New("provider ID does not match a configured provider")
)

// ProbeRuntime stores public settings for a diagnostic provider call.
//
// For example:
//
//	runtime, err := ReadProbeRuntime("runtime.json")
type ProbeRuntime struct {
	Providers          []json.RawMessage              `json:"PROVIDERS"`
	FailurePolicy      config.ProviderFailurePolicy   `json:"PROVIDER_FAILURE_POLICY"`
	Minimum            string                         `json:"REVIEW_MIN_IMPORTANCE"`
	Importance         reviewrules.Importance         `json:"REVIEW_RULE_IMPORTANCE"`
	Pricing            map[string]config.ModelPricing `json:"REVIEW_MODEL_PRICING"`
	BudgetURL          string                         `json:"PROVIDER_BUDGET_URL"`
	Timeout            string                         `json:"REVIEW_CHUNK_TIMEOUT"`
	Concurrency        *string                        `json:"REVIEW_CHUNK_CONCURRENCY"`
	PromptBytes        *string                        `json:"REVIEW_MAX_PROMPT_BYTES"`
	RulesFile          string                         `json:"REVIEW_RULES_FILE"`
	PromptsFile        string                         `json:"REVIEW_PROMPTS_FILE"`
	ServiceFile        string                         `json:"REVIEW_SERVICE_FILE"`
	TemplatesDirectory string                         `json:"REVIEW_TEMPLATES_DIR"`
}

// ProbeConfig loads one configured provider and its review policy.
//
// For example:
//
//	cfg, err := ProbeConfig(runtime, "provider", credential, signingKeyPath)
func ProbeConfig(
	runtime ProbeRuntime,
	providerID string,
	credential string,
	signingKeyPath string,
) (
	config.Config,
	error,
) {
	var cfg config.Config

	providers, err := loadProbeProviders(runtime, providerID, credential)
	if err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return cfg, fmt.Errorf("load probe configuration: %w", err)
	}

	cfg.Providers = providers

	cfg.ProviderFailurePolicy, err = config.LoadProviderFailurePolicy(
		func(name string) (string, bool) {
			return string(runtime.FailurePolicy), name == "PROVIDER_FAILURE_POLICY"
		},
	)
	if err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return cfg, fmt.Errorf("load probe configuration: %w", err)
	}

	reviewLimitValues := make(map[string]string)
	if runtime.Concurrency != nil {
		reviewLimitValues["REVIEW_CHUNK_CONCURRENCY"] = *runtime.Concurrency
	}

	if runtime.PromptBytes != nil {
		reviewLimitValues["REVIEW_MAX_PROMPT_BYTES"] = *runtime.PromptBytes
	}

	if err = config.LoadReviewLimits(
		func(name string) (string, bool) {
			value, configured := reviewLimitValues[name]

			return value, configured
		},
		&cfg,
	); err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return cfg, fmt.Errorf("load probe configuration: %w", err)
	}

	cfg.ReviewPolicy, err = reviewrules.LoadPolicy(
		runtime.RulesFile,
		runtime.PromptsFile,
		runtime.ServiceFile,
		runtime.TemplatesDirectory,
	)
	if err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return cfg, fmt.Errorf("load probe configuration: %w", err)
	}

	cfg.MinimumImportance, err = strconv.Atoi(runtime.Minimum)
	if err != nil || cfg.MinimumImportance < reviewrules.MinimumImportance ||
		cfg.MinimumImportance > reviewrules.MaximumImportance {
		return cfg, errInvalidImportance
	}

	if err = cfg.ReviewPolicy.Catalog().ValidateImportance(runtime.Importance); err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return cfg, fmt.Errorf("load probe configuration: %w", err)
	}

	cfg.RuleImportance = runtime.Importance
	cfg.ReviewModelPricing = runtime.Pricing

	cfg.ReviewChunkTimeout, err = time.ParseDuration(runtime.Timeout)
	if err != nil || cfg.ReviewChunkTimeout <= 0 {
		return cfg, errInvalidTimeout
	}

	if providers[0].HasTokenLimit() {
		if err := configureProbeBudget(&cfg, runtime, signingKeyPath); err != nil {
			return cfg, err
		}
	}

	return cfg, nil
}

// ReadProbeRuntime reads diagnostic settings and resolves relative paths.
//
// For example:
//
//	runtime, err := ReadProbeRuntime("runtime.json")
func ReadProbeRuntime(path string) (ProbeRuntime, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return ProbeRuntime{}, fmt.Errorf("read probe runtime: %w", err)
	}

	var runtime ProbeRuntime
	if err = json.Unmarshal(data, &runtime); err != nil {
		return runtime, errInvalidRuntime
	}

	for _, reference := range []*string{
		&runtime.RulesFile,
		&runtime.PromptsFile,
		&runtime.ServiceFile,
		&runtime.TemplatesDirectory,
	} {
		if *reference != "" && !filepath.IsAbs(*reference) {
			*reference = filepath.Join(filepath.Dir(path), *reference)
		}
	}

	return runtime, nil
}

func loadProbeProviders(
	runtime ProbeRuntime,
	providerID string,
	credential string,
) ([]config.ProviderConfig, error) {
	var selected json.RawMessage

	for _, definition := range runtime.Providers {
		var identity struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(definition, &identity); err != nil {
			return nil, errInvalidProvider
		}

		if identity.ID == providerID {
			if selected != nil {
				return nil, errDuplicateProvider
			}

			selected = definition
		}
	}

	if selected == nil {
		return nil, errUnknownProvider
	}

	definitions, err := json.Marshal([]json.RawMessage{selected})
	if err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return nil, fmt.Errorf("load probe providers: %w", err)
	}

	priority, err := json.Marshal([]string{providerID})
	if err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return nil, fmt.Errorf("load probe providers: %w", err)
	}

	providers, err := config.LoadProviders(func(name string) (string, bool) {
		switch name {
		case "PROVIDERS":
			return string(definitions), true
		case "PROVIDER_PRIORITY":
			return string(priority), true
		case "PROVIDER_" + strings.ToUpper(providerID) + "_API_KEY":
			return credential, true
		default:
			return os.LookupEnv(name)
		}
	})
	if err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return nil, fmt.Errorf("load probe providers: %w", err)
	}

	return providers, nil
}

func configureProbeBudget(cfg *config.Config, runtime ProbeRuntime, signingKeyPath string) error {
	var err error
	if signingKeyPath == "" {
		return errMissingSigningKey
	}

	cfg.ProviderBudgetURL, err = url.Parse(runtime.BudgetURL)
	if err != nil || cfg.ProviderBudgetURL.Scheme != "https" ||
		cfg.ProviderBudgetURL.Host == "" {
		return errInvalidBudgetURL
	}

	cfg.GitHubWebhookSecret, err = cloudflareops.ReadCredential(signingKeyPath)
	if err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return fmt.Errorf("load probe configuration: %w", err)
	}

	return nil
}

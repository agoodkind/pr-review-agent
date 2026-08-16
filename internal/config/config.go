// Package config loads strict runtime configuration from the environment.
package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// Model is the OpenAI model used for review and reconciliation.
	Model = "gpt-5.6-sol"
	// ReasoningEffort is the OpenAI reasoning effort for every completion.
	ReasoningEffort = "high"
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
	// GitHubAPIVersion is the GitHub REST API version sent on every request.
	GitHubAPIVersion = "2022-11-28"
	// WritingPolicy is injected into every review and reconciliation prompt.
	WritingPolicy = "Use clean GitHub Markdown. Give each finding one short heading and direct prose. Limit each finding to the defect, impact, and fix in at most three short sentences. Omit repetition, praise, introductions, numeric severity labels, unnecessary detail, progress messages, commands, replies, and typographic dashes."
)

// LookupEnv reads one environment variable.
type LookupEnv func(string) (string, bool)

// Config holds validated service configuration.
type Config struct {
	Port                      string
	ReviewTimeout             time.Duration
	ReviewWorkers             int
	MinimumImportance         int
	MaximumUnresolvedComments int
	GitHubAppID               int64
	GitHubPrivateKey          *rsa.PrivateKey
	GitHubWebhookSecret       []byte
	GitHubBotLogin            string
	GitHubAPIBaseURL          *url.URL
	GitHubGraphQLURL          *url.URL
	ClydeBaseURL              *url.URL
	ClydeAPIKey               string
	CFAccessClientID          string
	CFAccessClientSecret      string
}

// FromEnvironment loads configuration from process environment variables.
func FromEnvironment() (Config, error) {
	return Load(os.LookupEnv)
}

// Load validates configuration loaded through lookup.
func Load(lookup LookupEnv) (Config, error) {
	cfg, missing := loadBase(lookup)
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
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
	reviewTimeout, ok := loadReviewTimeout(lookup)
	if !ok {
		missing = append(missing, "REVIEW_TIMEOUT")
	} else {
		cfg.ReviewTimeout = reviewTimeout
	}
	minimumImportance, ok := loadMinimumImportance(lookup)
	if !ok {
		missing = append(missing, "REVIEW_MIN_IMPORTANCE")
	} else {
		cfg.MinimumImportance = minimumImportance
	}
	maximumComments, ok := loadMaximumUnresolvedComments(lookup)
	if !ok {
		missing = append(missing, "REVIEW_MAX_UNRESOLVED_COMMENTS")
	} else {
		cfg.MaximumUnresolvedComments = maximumComments
	}
	missing = append(missing, loadGitHub(lookup, &cfg)...)
	missing = append(missing, loadClyde(lookup, &cfg)...)

	return cfg, missing
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

func loadReviewTimeout(lookup LookupEnv) (time.Duration, bool) {
	value, ok := lookup("REVIEW_TIMEOUT")
	if !ok || strings.TrimSpace(value) == "" {
		return 0, false
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return 0, false
	}
	return timeout, true
}

func loadMaximumUnresolvedComments(lookup LookupEnv) (int, bool) {
	value, ok := lookup("REVIEW_MAX_UNRESOLVED_COMMENTS")
	if !ok || strings.TrimSpace(value) == "" {
		return 0, false
	}
	maximum, err := strconv.Atoi(value)
	if err != nil || maximum < 0 {
		return 0, false
	}
	return maximum, true
}

func loadMinimumImportance(lookup LookupEnv) (int, bool) {
	value, ok := lookup("REVIEW_MIN_IMPORTANCE")
	if !ok || strings.TrimSpace(value) == "" {
		return 0, false
	}
	importance, err := strconv.Atoi(value)
	if err != nil || importance < 1 || importance > 10 {
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

	cfClientID, ok := lookup("CF_ACCESS_CLIENT_ID")
	if !ok || strings.TrimSpace(cfClientID) == "" {
		missing = append(missing, "CF_ACCESS_CLIENT_ID")
	} else {
		cfg.CFAccessClientID = cfClientID
	}

	cfAccessValue, ok := lookup("CF_ACCESS_CLIENT_SECRET")
	if !ok || strings.TrimSpace(cfAccessValue) == "" {
		missing = append(missing, "CF_ACCESS_CLIENT_SECRET")
	} else {
		cfg.CFAccessClientSecret = cfAccessValue // gitleaks:allow
	}

	return missing
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

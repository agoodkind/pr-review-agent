package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

const testBotLogin = "test-review-agent[bot]"

func TestEndToEndLoadEnablesReviewRuntime(t *testing.T) {
	cfg, err := loadWithOverrides(map[string]string{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubPrivateKey == nil {
		t.Fatal("GitHubPrivateKey is nil")
	}
	if len(cfg.GitHubWebhookSecret) == 0 {
		t.Fatal("GitHubWebhookSecret is empty")
	}
	if cfg.ClydeBaseURL == nil || cfg.ClydeBaseURL.String() == "" {
		t.Fatal("ClydeBaseURL is empty")
	}
	if cfg.GitHubAPIBaseURL == nil || cfg.GitHubAPIBaseURL.String() == "" {
		t.Fatal("GitHubAPIBaseURL is empty")
	}
}

func TestLoadRequiresEverySecret(t *testing.T) {
	_, err := Load(func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("empty env: want error")
	}
	for _, secret := range []string{
		"GITHUB_APP_ID",
		"GITHUB_PRIVATE_KEY",
		"GITHUB_WEBHOOK_SECRET",
		"GITHUB_BOT_LOGIN",
		"CLYDE_BASE_URL",
		"CLYDE_API_KEY",
		"CF_ACCESS_CLIENT_ID",
		"CF_ACCESS_CLIENT_SECRET",
		"REVIEW_MIN_IMPORTANCE",
		"REVIEW_MAX_UNRESOLVED_COMMENTS",
		"REVIEW_TIMEOUT",
		"REVIEW_WORKERS",
		"REVIEW_MODEL",
	} {
		if !strings.Contains(err.Error(), secret) {
			t.Fatalf("error %q missing %q", err.Error(), secret)
		}
	}
}

func TestLoadWithoutFallbackLeavesTheFallbackUnset(t *testing.T) {
	cfg, err := loadWithOverrides(map[string]string{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HasFallback() {
		t.Fatal("HasFallback() = true, want false when no fallback is configured")
	}
	if cfg.FallbackBaseURL != nil {
		t.Fatalf("FallbackBaseURL = %v, want nil", cfg.FallbackBaseURL)
	}
	if cfg.FallbackModel != "" || cfg.FallbackAPIKey != "" {
		t.Fatalf("fallback model = %q, key set = %t, want both empty", cfg.FallbackModel, cfg.FallbackAPIKey != "")
	}
	if cfg.FallbackOnUsageExceeded {
		t.Fatal("FallbackOnUsageExceeded = true, want false without a fallback")
	}
}

func TestLoadReadsACompleteFallback(t *testing.T) {
	cfg, err := loadWithOverrides(completeFallback(map[string]string{}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.HasFallback() {
		t.Fatal("HasFallback() = false, want true")
	}
	if cfg.FallbackBaseURL.String() != "https://fallback.example" {
		t.Fatalf("FallbackBaseURL = %q, want https://fallback.example", cfg.FallbackBaseURL)
	}
	if cfg.FallbackModel != "fixture-fallback-model" {
		t.Fatalf("FallbackModel = %q, want fixture-fallback-model", cfg.FallbackModel)
	}
	if cfg.FallbackAPIKey == "" {
		t.Fatal("FallbackAPIKey is empty")
	}
	if !cfg.FallbackOnUsageExceeded {
		t.Fatal("FallbackOnUsageExceeded = false, want true by default")
	}
	if cfg.FallbackCFAccessClientID != "" || cfg.FallbackCFAccessClientSecret != "" {
		t.Fatal("Cloudflare Access values are set although none were configured")
	}
}

func TestLoadRejectsAPartialFallback(t *testing.T) {
	cases := map[string][]string{
		"FALLBACK_BASE_URL": {"FALLBACK_MODEL", "FALLBACK_API_KEY"},
		"FALLBACK_MODEL":    {"FALLBACK_BASE_URL", "FALLBACK_API_KEY"},
		"FALLBACK_API_KEY":  {"FALLBACK_BASE_URL", "FALLBACK_MODEL"},
	}
	for present, wantMissing := range cases {
		t.Run(present, func(t *testing.T) {
			only := map[string]string{present: completeFallback(map[string]string{})[present]}
			_, err := loadWithOverrides(only)
			if err == nil {
				t.Fatalf("Load with only %s: want error", present)
			}
			for _, name := range wantMissing {
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("error %q missing %q", err.Error(), name)
				}
			}
		})
	}
}

func TestLoadRejectsAPlaintextFallbackURL(t *testing.T) {
	_, err := loadWithOverrides(completeFallback(map[string]string{
		"FALLBACK_BASE_URL": "http://fallback.example",
	}))
	if err == nil || !strings.Contains(err.Error(), "FALLBACK_BASE_URL") {
		t.Fatalf("Load with plaintext fallback URL: err = %v, want FALLBACK_BASE_URL", err)
	}
}

func TestLoadRequiresBothFallbackAccessValuesTogether(t *testing.T) {
	t.Run("both set", func(t *testing.T) {
		cfg, err := loadWithOverrides(completeFallback(map[string]string{
			"FALLBACK_CF_ACCESS_CLIENT_ID":     "fixture-fallback-cf-id",
			"FALLBACK_CF_ACCESS_CLIENT_SECRET": "fixture-fallback-cf-" + strings.Repeat("d", 8),
		}))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.FallbackCFAccessClientID != "fixture-fallback-cf-id" {
			t.Fatalf("FallbackCFAccessClientID = %q", cfg.FallbackCFAccessClientID)
		}
		if cfg.FallbackCFAccessClientSecret == "" {
			t.Fatal("FallbackCFAccessClientSecret is empty")
		}
	})

	for _, present := range []string{"FALLBACK_CF_ACCESS_CLIENT_ID", "FALLBACK_CF_ACCESS_CLIENT_SECRET"} {
		t.Run("only "+present, func(t *testing.T) {
			_, err := loadWithOverrides(completeFallback(map[string]string{present: "fixture-value"}))
			if err == nil {
				t.Fatalf("Load with only %s: want error", present)
			}
		})
	}
}

func TestLoadRejectsAnUnknownFallbackTrigger(t *testing.T) {
	_, err := loadWithOverrides(completeFallback(map[string]string{"FALLBACK_ON": "any_error"}))
	if err == nil || !strings.Contains(err.Error(), "FALLBACK_ON") {
		t.Fatalf("Load with unknown trigger: err = %v, want FALLBACK_ON", err)
	}
}

func TestLoadAcceptsTheUsageExceededTrigger(t *testing.T) {
	cfg, err := loadWithOverrides(completeFallback(map[string]string{"FALLBACK_ON": FallbackOnUsageExceeded}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.FallbackOnUsageExceeded {
		t.Fatal("FallbackOnUsageExceeded = false, want true")
	}
}

func TestFallbackFailureNeverNamesASecretValue(t *testing.T) {
	fallbackKey := "fixture-fallback-key-" + strings.Repeat("z", 12)
	_, err := loadWithOverrides(map[string]string{"FALLBACK_API_KEY": fallbackKey})
	if err == nil {
		t.Fatal("Load with only FALLBACK_API_KEY: want error")
	}
	if strings.Contains(err.Error(), fallbackKey) {
		t.Fatal("error names the fallback key value")
	}
}

func completeFallback(overrides map[string]string) map[string]string {
	values := map[string]string{
		"FALLBACK_BASE_URL": "https://fallback.example",
		"FALLBACK_MODEL":    "fixture-fallback-model",
		"FALLBACK_API_KEY":  "fixture-fallback-" + strings.Repeat("e", 8),
	}
	for key, value := range overrides {
		values[key] = value
	}
	return values
}

func TestLoadParsesPKCS1AndPKCS8RSAKeys(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	pkcs1 := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}))
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	pkcs8 := string(pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8Bytes,
	}))

	for _, keyText := range []string{pkcs1, pkcs8} {
		cfg, err := loadWithKey(keyText)
		if err != nil {
			t.Fatalf("Load with key: %v", err)
		}
		if cfg.GitHubPrivateKey == nil {
			t.Fatal("GitHubPrivateKey is nil")
		}
	}
}

func TestLoadRejectsNonRSAAndMalformedKeys(t *testing.T) {
	cases := map[string]string{
		"GITHUB_PRIVATE_KEY": "not-pem",
	}
	for key, value := range cases {
		_, err := loadWithOverrides(map[string]string{key: value})
		if err == nil {
			t.Fatalf("%s malformed: want error", key)
		}
	}
}

func TestLoadUsesPortAndGitHubURLDefaults(t *testing.T) {
	cfg, err := loadWithOverrides(map[string]string{})
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Port != "3000" {
		t.Fatalf("Port = %q, want 3000", cfg.Port)
	}
	if cfg.GitHubBotLogin != testBotLogin {
		t.Fatalf("GitHubBotLogin = %q, want %q", cfg.GitHubBotLogin, testBotLogin)
	}
	if cfg.GitHubAPIBaseURL.String() != "https://api.github.com" {
		t.Fatalf("GitHubAPIBaseURL = %q", cfg.GitHubAPIBaseURL)
	}
	if cfg.GitHubGraphQLURL.String() != "https://api.github.com/graphql" {
		t.Fatalf("GitHubGraphQLURL = %q", cfg.GitHubGraphQLURL)
	}
}

func TestLoadUsesConfiguredMinimumImportance(t *testing.T) {
	cfg, err := loadWithOverrides(map[string]string{"REVIEW_MIN_IMPORTANCE": "9"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MinimumImportance != 9 {
		t.Fatalf("MinimumImportance = %d, want 9", cfg.MinimumImportance)
	}
}

func TestLoadUsesConfiguredMaximumUnresolvedComments(t *testing.T) {
	cfg, err := loadWithOverrides(map[string]string{"REVIEW_MAX_UNRESOLVED_COMMENTS": "12"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaximumUnresolvedComments != 12 {
		t.Fatalf("MaximumUnresolvedComments = %d, want 12", cfg.MaximumUnresolvedComments)
	}
}

func TestLoadUsesConfiguredReviewTimeout(t *testing.T) {
	cfg, err := loadWithOverrides(map[string]string{"REVIEW_TIMEOUT": "12m30s"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReviewTimeout != 12*time.Minute+30*time.Second {
		t.Fatalf("ReviewTimeout = %s, want 12m30s", cfg.ReviewTimeout)
	}
}

func TestLoadUsesConfiguredReviewWorkers(t *testing.T) {
	cfg, err := loadWithOverrides(map[string]string{"REVIEW_WORKERS": "4"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReviewWorkers != 4 {
		t.Fatalf("ReviewWorkers = %d, want 4", cfg.ReviewWorkers)
	}
}

func TestLoadRejectsInvalidURLsAndAppIDs(t *testing.T) {
	_, err := loadWithOverrides(map[string]string{
		"GITHUB_APP_ID":  "0",
		"CLYDE_BASE_URL": "not-a-url",
	})
	if err == nil {
		t.Fatal("invalid app id and URL: want error")
	}
}

func TestLoadRejectsPlaintextClydeURL(t *testing.T) {
	_, err := loadWithOverrides(map[string]string{
		"CLYDE_BASE_URL": "http://clyde.example",
	})
	if err == nil {
		t.Fatal("plaintext Clyde URL: want error")
	}
}

func TestLoadErrorsDoNotContainSecretValues(t *testing.T) {
	secret := "fixture-" + strings.Repeat("z", 12)
	_, err := loadWithOverrides(map[string]string{
		"GITHUB_WEBHOOK_SECRET": "",
		"CLYDE_API_KEY":         secret,
	})
	if err == nil {
		t.Fatal("missing webhook secret: want error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaks secret: %q", err.Error())
	}
}

func loadWithKey(privateKey string) (Config, error) {
	return loadWithOverrides(map[string]string{
		"GITHUB_PRIVATE_KEY": privateKey,
	})
}

func loadWithOverrides(overrides map[string]string) (Config, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return Config{}, err
	}
	defaultKey := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}))

	values := map[string]string{
		"GITHUB_APP_ID":                  "12345",
		"GITHUB_PRIVATE_KEY":             defaultKey,
		"GITHUB_WEBHOOK_SECRET":          "fixture-webhook-" + strings.Repeat("a", 8),
		"GITHUB_BOT_LOGIN":               testBotLogin,
		"CLYDE_BASE_URL":                 "https://clyde.example",
		"CLYDE_API_KEY":                  "fixture-clyde-" + strings.Repeat("b", 8),
		"CF_ACCESS_CLIENT_ID":            "fixture-cf-id",
		"CF_ACCESS_CLIENT_SECRET":        "fixture-cf-" + strings.Repeat("c", 8),
		"REVIEW_MIN_IMPORTANCE":          "7",
		"REVIEW_MAX_UNRESOLVED_COMMENTS": "10",
		"REVIEW_TIMEOUT":                 "10m",
		"REVIEW_WORKERS":                 "4",
		"REVIEW_MODEL":                   "fixture-primary-model",
	}
	for key, value := range overrides {
		values[key] = value
	}

	return Load(func(key string) (string, bool) {
		value, ok := values[key]
		if !ok {
			return "", false
		}
		return value, true
	})
}

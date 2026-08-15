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
	} {
		if !strings.Contains(err.Error(), secret) {
			t.Fatalf("error %q missing %q", err.Error(), secret)
		}
	}
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

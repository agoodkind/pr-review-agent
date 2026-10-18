package config_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
)

func TestRepositoryRuntimeLoadsServiceConfiguration(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	data, err := os.ReadFile("runtime.json")
	if err != nil {
		t.Fatal(err)
	}
	var runtime struct {
		Providers []struct {
			ID                    string `json:"id"`
			AccessClientIDBinding string `json:"cf_access_client_id_binding"`
			AccessSecretBinding   string `json:"cf_access_client_secret_binding"`
		} `json:"PROVIDERS"`
	}
	if err := json.Unmarshal(data, &runtime); err != nil {
		t.Fatal(err)
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	credentials := map[string]string{
		"GITHUB_PRIVATE_KEY": string(pem.EncodeToMemory(&pem.Block{
			Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
		})),
		"GITHUB_WEBHOOK_SECRET": "fixture-runtime-webhook-secret",
	}
	for _, provider := range runtime.Providers {
		prefix := "PROVIDER_" + strings.ToUpper(provider.ID)
		credentials[prefix+"_API_KEY"] = "fixture-runtime-api-key"
		if provider.AccessClientIDBinding != "" {
			credentials[prefix+"_CF_ACCESS_CLIENT_ID"] = "fixture-runtime-access-id"
		}
		if provider.AccessSecretBinding != "" {
			credentials[prefix+"_CF_ACCESS_CLIENT_SECRET"] = "fixture-runtime-access-secret"
		}
	}
	cfg, err := config.LoadRuntime(data, func(name string) (string, bool) {
		value, found := credentials[name]
		return value, found
	})
	if err != nil {
		t.Fatalf("load repository runtime: %v", err)
	}
	if cfg.GitHubPrivateKey == nil || len(cfg.GitHubWebhookSecret) == 0 ||
		len(cfg.Providers) == 0 || len(cfg.Providers) != len(runtime.Providers) {
		t.Fatal("repository runtime did not configure the service credentials and providers")
	}
}

package config_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/quota"
)

func TestRuntimeBudgetReportPolicy(t *testing.T) {
	t.Chdir("../..")
	values := make(map[string]json.RawMessage)
	for name, value := range map[string]string{
		"GITHUB_APP_ID": "12345", "GITHUB_BOT_LOGIN": "configuration-policy-test[bot]",
		"CLYDE_BASE_URL": "https://provider.example/v1", "REVIEW_MODEL": "configuration-policy-test",
		"REVIEW_WORKERS": "1", "REVIEW_MIN_IMPORTANCE": "7",
		"REVIEW_RULES_FILE": "config/review-rules.json", "REVIEW_PROMPTS_FILE": "config/review-prompts.json",
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		values[name] = encoded
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_PRIVATE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})))
	t.Setenv("GITHUB_WEBHOOK_SECRET", "configuration-policy-test")
	t.Setenv("CLYDE_API_KEY", "configuration-policy-test")
	for _, example := range []struct {
		name   string
		raw    string
		policy quota.ReportPolicy
	}{
		{name: "omitted defaults", raw: "", policy: quota.ReportPolicy{Attempts: 3, AttemptTimeout: 5 * time.Second, Timeout: 20 * time.Second}},
		{name: "runtime overrides", raw: `{"attempts":1,"attempt_timeout":"25ms","timeout":"100ms"}`, policy: quota.ReportPolicy{Attempts: 1, AttemptTimeout: 25 * time.Millisecond, Timeout: 100 * time.Millisecond}},
	} {
		t.Run(example.name, func(t *testing.T) {
			if example.raw != "" {
				values["PROVIDER_BUDGET_REPORT"] = json.RawMessage(example.raw)
			}
			data, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadRuntime(data, os.LookupEnv)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.BudgetReportPolicy() != example.policy {
				t.Fatalf("runtime report policy = %+v, want %+v", cfg.BudgetReportPolicy(), example.policy)
			}
		})
	}
	for _, invalid := range []string{
		`null`, `[]`, `{}`, `{"attempts":1}`, `{"attempts":1,"attempt_timeout":"5s","timeout":"20s","other":true}`,
		`{"attempts":0,"attempt_timeout":"5s","timeout":"20s"}`,
		`{"attempts":-1,"attempt_timeout":"5s","timeout":"20s"}`,
		`{"attempts":1.5,"attempt_timeout":"5s","timeout":"20s"}`,
		`{"attempts":1,"attempt_timeout":"bad","timeout":"20s"}`,
		`{"attempts":1,"attempt_timeout":"0s","timeout":"20s"}`,
		`{"attempts":1,"attempt_timeout":"5s","timeout":"bad"}`,
		`{"attempts":1,"attempt_timeout":"5s","timeout":"1s"}`,
	} {
		values["PROVIDER_BUDGET_REPORT"] = json.RawMessage(invalid)
		data, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := config.LoadRuntime(data, os.LookupEnv); err == nil || !strings.Contains(err.Error(), "PROVIDER_BUDGET_REPORT") {
			t.Fatalf("invalid report policy %s produced %v", invalid, err)
		}
	}
}

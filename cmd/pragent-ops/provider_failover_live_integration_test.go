//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/modelrequest"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestLiveUnexpectedFailureSelectsNextProvider(t *testing.T) {
	cfg := liveFailoverConfig(t)
	selected := cfg.Providers[0]
	invalid := selected
	invalid.ID = "invalid_credentials"
	invalid.APIKey = rand.Text()
	invalid.RequestTimeout = 10 * time.Second
	selected.RequestTimeout = 10 * time.Second
	cfg.Providers = []config.ProviderConfig{invalid, selected}

	for _, stage := range []string{"review", "report", "reconcile", "consolidate"} {
		t.Run(stage, func(t *testing.T) {
			client := openai.NewClient(cfg, &http.Client{})
			ctx, cancel := modelrequest.WithTimeout(t.Context(), client, time.Nanosecond)
			defer cancel()
			ctx = gklog.WithLogger(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			ctx, recorder := review.WithUsageRecorder(ctx)
			_ = callLiveStage(ctx, client, stage, liveFailoverPrompt(t, cfg))
			attempts := recorder.Summary().ProviderAttempts
			if len(attempts) != 2 || attempts[0].Status.ProviderID != invalid.ID || attempts[1].Status.ProviderID != selected.ID {
				t.Fatal("the unexpected authentication failure did not select the next configured provider")
			}
			if attempts[0].HTTPStatus < 400 || attempts[0].HTTPStatus >= 500 || attempts[0].HTTPStatus == http.StatusTooManyRequests || !attempts[0].Fallback {
				t.Fatal("the first provider did not produce an unexpected client error followed by fallback")
			}
			if ctx.Err() != nil {
				t.Fatal("the inherited stage timeout cancelled independently bounded provider attempts")
			}
			assertFallbackResponded(t, recorder.Summary(), selected.ID)
		})
	}
}

func TestLiveProviderTimeoutSelectsNextProvider(t *testing.T) {
	cfg := liveFailoverConfig(t)
	selected := cfg.Providers[0]
	expired := selected
	expired.ID = "expired_attempt"
	expired.RequestTimeout = time.Nanosecond
	selected.RequestTimeout = 10 * time.Second
	cfg.Providers = []config.ProviderConfig{expired, selected}
	client := openai.NewClient(cfg, &http.Client{})
	ctx := gklog.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ctx, recorder := review.WithUsageRecorder(ctx)
	_, _ = client.Review(ctx, liveFailoverPrompt(t, cfg))
	attempts := recorder.Summary().ProviderAttempts
	if len(attempts) != 2 || attempts[0].Status.ProviderID != expired.ID || attempts[1].Status.ProviderID != selected.ID || !attempts[0].Fallback {
		t.Fatal("the provider timeout did not select the next provider")
	}
	if ctx.Err() != nil {
		t.Fatal("the provider timeout cancelled the parent request")
	}
	assertFallbackResponded(t, recorder.Summary(), selected.ID)
}

func assertFallbackResponded(t *testing.T, summary review.UsageSummary, providerID string) {
	t.Helper()
	attempt := summary.ProviderAttempts[len(summary.ProviderAttempts)-1]
	responded := attempt.HTTPStatus >= http.StatusBadRequest || attempt.Completed
	for _, usage := range summary.Models {
		if usage.ProviderID == providerID && usage.ReportedRequests > 0 {
			responded = true
		}
	}
	if !responded {
		t.Fatal("the fallback attempt did not receive a provider response")
	}
}

func TestCancelledRequestStartsNoProvider(t *testing.T) {
	cfg := liveFailoverConfig(t)
	client := openai.NewClient(cfg, &http.Client{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ctx = gklog.WithLogger(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ctx, recorder := review.WithUsageRecorder(ctx)
	_, err := client.Review(ctx, liveFailoverPrompt(t, cfg))
	if !errors.Is(err, context.Canceled) || len(recorder.Summary().ProviderAttempts) != 0 {
		t.Fatal("the cancelled request selected a provider")
	}
}

func liveFailoverConfig(t *testing.T) config.Config {
	t.Helper()
	providerID := os.Getenv("PR_AGENT_LIVE_PROVIDER_ID")
	credentialPath := os.Getenv("PR_AGENT_LIVE_API_KEY_FILE")
	if providerID == "" && credentialPath == "" {
		t.Skip("Live provider credentials are not configured.")
	}
	if providerID == "" || credentialPath == "" {
		t.Fatal("Live fallback verification requires a configured provider and credential file.")
	}
	runtime, err := ReadProbeRuntime(filepath.Join("..", "..", "runtime.json"))
	if err != nil {
		t.Fatal("The runtime configuration could not be read.")
	}
	credential, err := cloudflareops.ReadCredential(credentialPath)
	if err != nil {
		t.Fatal("The credential file could not be read.")
	}
	cfg, err := probeConfig(runtime, providerID, string(credential), "")
	if err != nil || len(cfg.Providers) != 1 || cfg.Providers[0].HasTokenLimit() {
		t.Fatal("Fallback verification requires an existing provider without an app quota.")
	}
	if cfg.ProviderFailurePolicy != config.ProviderAnyFailure {
		t.Fatal("The configured provider failure policy does not permit unexpected-error fallback.")
	}
	return cfg
}

func liveFailoverPrompt(t *testing.T, cfg config.Config) string {
	t.Helper()
	prompt, err := cfg.ReviewPolicy.Render("probe.user", reviewrules.PromptData{})
	if err != nil {
		t.Fatal("The configured probe prompt could not be rendered.")
	}
	return prompt
}

func callLiveStage(ctx context.Context, client *openai.Client, stage string, prompt string) error {
	switch stage {
	case "review":
		_, err := client.Review(ctx, prompt)
		return err
	case "report":
		_, err := client.Report(ctx, prompt)
		return err
	case "reconcile":
		_, err := client.Reconcile(ctx, prompt)
		return err
	case "consolidate":
		_, err := client.Consolidate(ctx, prompt)
		return err
	default:
		return errors.New("the live verification stage is invalid")
	}
}

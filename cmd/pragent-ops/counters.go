package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"goodkind.io/pr-review-agent/internal/clock"
	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/quota"
)

type publicProvider struct {
	ID              string            `json:"id"`
	Model           string            `json:"model"`
	Disabled        bool              `json:"disabled"`
	DailyTokenLimit int64             `json:"daily_token_limit"`
	TokenLimit      int64             `json:"token_limit"`
	TokenTypes      []quota.TokenType `json:"token_types"`
	Window          quota.Window      `json:"token_window"`
	TokenLimits     []quota.Limit     `json:"token_limits"`
}
type counterRuntime struct {
	URL       string           `json:"PROVIDER_BUDGET_URL"`
	Providers []publicProvider `json:"PROVIDERS"`
}
type providerCounter struct {
	Provider        string               `json:"provider"`
	Model           string               `json:"model"`
	ConfiguredLimit int64                `json:"configured_limit"`
	Legacy          *quota.DailySnapshot `json:"legacy_daily_snapshot,omitempty"`
	Window          *quota.Bounds        `json:"window,omitempty"`
	History         *quota.QueryResponse `json:"history,omitempty"`
	Snapshot        *quota.Snapshot      `json:"snapshot,omitempty"`
	TokenLimits     []windowCounter      `json:"token_limits,omitempty"`
}

type windowCounter struct {
	Configuration quota.Limit         `json:"configuration"`
	Window        quota.Bounds        `json:"window"`
	History       quota.QueryResponse `json:"history"`
	Snapshot      quota.Snapshot      `json:"snapshot"`
}
type counterCapture struct {
	CapturedAt  time.Time         `json:"captured_at"`
	Runtime     string            `json:"runtime"`
	Counters    []providerCounter `json:"counters"`
	Limitations []string          `json:"limitations"`
}

func (capture counterCapture) MarshalJSON() ([]byte, error) {
	type wireCapture counterCapture
	data, err := json.Marshal(wireCapture(capture))
	if err != nil {
		return nil, fmt.Errorf("encode counter capture: %w", err)
	}
	return data, nil
}

func counterClient(endpoint, signingKeyPath, operatorTokenPath string) (*quota.Client, error) {
	credentialPath := signingKeyPath
	if operatorTokenPath != "" {
		credentialPath = operatorTokenPath
	}
	key, err := cloudflareops.ReadCredential(credentialPath)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	if operatorTokenPath != "" {
		return quota.NewOperatorClient(endpoint, key, httpClient)
	}
	return quota.NewClient(endpoint, key, httpClient), nil
}

func counters(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	var runtimePath, signingKeyPath, operatorTokenPath, output, providerID string
	set := flag.NewFlagSet("counters", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&runtimePath, "runtime", "runtime.json", "Public provider runtime configuration.")
	set.StringVar(&signingKeyPath, "signing-key-file", "", "File containing the production provider-budget HMAC key.")
	set.StringVar(&operatorTokenPath, "operator-token-file", "", "File containing the registered operator token for counter reads.")
	set.StringVar(&output, "output-dir", "", "New private output directory; defaults to a private temporary directory.")
	set.StringVar(&providerID, "provider", "", "Optional exact provider ID; defaults to every enabled capped provider.")
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if set.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if (signingKeyPath == "") == (operatorTokenPath == "") {
		return errors.New("counters requires exactly one --operator-token-file or --signing-key-file")
	}
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		slog.WarnContext(ctx, "Public runtime configuration read failed", "path", runtimePath)
		return fmt.Errorf("read public runtime configuration: %w", err)
	}
	var runtime counterRuntime
	if json.Unmarshal(data, &runtime) != nil || runtime.URL == "" || len(runtime.Providers) == 0 {
		return errors.New("runtime configuration requires PROVIDER_BUDGET_URL and PROVIDERS")
	}
	client, err := counterClient(runtime.URL, signingKeyPath, operatorTokenPath)
	if err != nil {
		return err
	}
	var capture counterCapture
	capture.CapturedAt = clock.System().UTC()
	capture.Runtime = runtimePath
	capture.Counters = []providerCounter{}
	capture.Limitations = []string{"Legacy snapshots cover the current UTC day only. Event history starts at the stored history_start_ms; earlier history is not fabricated.", "A history query may initialize the provider history boundary. The command does not report usage or change token limits."}
	for _, provider := range runtime.Providers {
		if providerID != "" && provider.ID != providerID {
			continue
		}
		if provider.Disabled && providerID == "" {
			continue
		}
		if provider.ID == "" || provider.Model == "" {
			return errors.New("runtime provider ID and model are required")
		}
		if provider.TokenLimit > 0 && provider.DailyTokenLimit > 0 {
			return errors.New("runtime provider configures both legacy and generic token limits")
		}
		var result providerCounter
		result.Provider = provider.ID
		result.Model = provider.Model
		switch {
		case provider.TokenLimit > 0:
			counter, counterErr := readWindowCounter(ctx, client, provider, capture.CapturedAt, quota.Limit{
				Limit: provider.TokenLimit, TokenTypes: provider.TokenTypes, Window: provider.Window,
			})
			if counterErr != nil {
				return counterErr
			}
			result.ConfiguredLimit = provider.TokenLimit
			result.Window = &counter.Window
			result.History = &counter.History
			result.Snapshot = &counter.Snapshot
		case provider.DailyTokenLimit > 0:
			snapshot, queryErr := client.CheckDaily(ctx, provider.ID, provider.Model, provider.DailyTokenLimit)
			if queryErr != nil {
				slog.WarnContext(ctx, "Provider daily snapshot query failed", "provider", provider.ID)
				return fmt.Errorf("provider %s daily snapshot failed: %w", provider.ID, queryErr)
			}
			result.ConfiguredLimit = provider.DailyTokenLimit
			result.Legacy = &snapshot
		default:
			if len(provider.TokenLimits) == 0 {
				continue
			}
		}
		for _, limit := range provider.TokenLimits {
			counter, counterErr := readWindowCounter(ctx, client, provider, capture.CapturedAt, limit)
			if counterErr != nil {
				return counterErr
			}
			result.TokenLimits = append(result.TokenLimits, counter)
		}
		capture.Counters = append(capture.Counters, result)
	}
	if len(capture.Counters) == 0 {
		return errors.New("runtime selection contains no enabled capped provider")
	}
	directory, err := cloudflareops.PrivateDirectory(output)
	if err != nil {
		return err
	}
	path := filepath.Join(directory, "counters.json")
	if err = cloudflareops.WriteJSON(path, capture); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Provider counter snapshots: %d. Captured at: %s.\nPrivate counters: %s\n", len(capture.Counters), capture.CapturedAt.Format(time.RFC3339), path)
	return err
}

func readWindowCounter(ctx context.Context, client *quota.Client, provider publicProvider, now time.Time, limit quota.Limit) (windowCounter, error) {
	var result windowCounter
	result.Configuration = limit
	if err := limit.Validate(); err != nil {
		slog.WarnContext(ctx, "Provider token limit validation failed", "provider", provider.ID)
		return result, fmt.Errorf("provider %s token limit is invalid: %w", provider.ID, err)
	}
	var err error
	result.Window, err = limit.Window.Bounds(now)
	if err != nil {
		slog.WarnContext(ctx, "Provider token window validation failed", "provider", provider.ID)
		return result, fmt.Errorf("provider %s token window is invalid: %w", provider.ID, err)
	}
	result.History, err = client.Query(ctx, provider.ID, provider.Model, result.Window)
	if err != nil {
		slog.WarnContext(ctx, "Provider counter query failed", "provider", provider.ID)
		return result, fmt.Errorf("provider %s counter query failed: %w", provider.ID, err)
	}
	result.Snapshot, err = quota.Evaluate(limit.Window, now, limit.Limit, limit.TokenTypes, result.History.Events, result.History.HistoryStartMS)
	if err != nil {
		slog.WarnContext(ctx, "Provider counter evaluation failed", "provider", provider.ID)
		return result, fmt.Errorf("provider %s counter evaluation failed: %w", provider.ID, err)
	}
	return result, nil
}

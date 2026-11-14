package main

import (
	"bytes"
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
	"strings"
	"sync"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/modelrequest"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/opsruntime"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

const probeErrorBodyLimit = 32 * 1024

type probeHTTPError struct {
	Status    int    `json:"status"`
	Body      string `json:"body_prefix"`
	Truncated bool   `json:"truncated"`
}

type probeTransport struct {
	base       http.RoundTripper
	mu         sync.Mutex
	statuses   map[int]int
	errors     []probeHTTPError
	captureErr error
}

type probeResponseBody struct {
	io.Reader
	io.Closer
}

func (transport *probeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return response, err
	}

	transport.mu.Lock()
	defer transport.mu.Unlock()

	transport.statuses[response.StatusCode]++
	if response.StatusCode < http.StatusBadRequest {
		return response, nil
	}
	// The SDK must receive the consumed prefix followed by the unread body.
	prefix, captureErr := io.ReadAll(io.LimitReader(response.Body, probeErrorBodyLimit+1))
	response.Body = probeResponseBody{
		Reader: io.MultiReader(bytes.NewReader(prefix), response.Body),
		Closer: response.Body,
	}
	transport.captureErr = errors.Join(transport.captureErr, captureErr)
	transport.errors = append(
		transport.errors,
		probeHTTPError{
			Status:    response.StatusCode,
			Body:      string(prefix[:min(len(prefix), probeErrorBodyLimit)]),
			Truncated: len(prefix) > probeErrorBodyLimit,
		},
	)

	return response, nil
}

func probeConfig(
	runtime opsruntime.ProbeRuntime,
	providerID string,
	credential string,
	signingKeyPath string,
) (
	config.Config,
	error,
) {
	return opsruntime.ProbeConfig(runtime, providerID, credential, signingKeyPath)
}

func probe(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	var providerID, credentialPath, runtimePath, promptPath, directoryPath, signingKeyPath string

	set := flag.NewFlagSet("probe", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&providerID, "provider", "", "Exact provider ID in the runtime configuration.")
	set.StringVar(
		&credentialPath,
		"credential-file",
		"",
		"Private file containing the selected provider API key.",
	)
	set.StringVar(&runtimePath, "runtime", "runtime.json", "Public provider runtime configuration.")
	set.StringVar(
		&promptPath,
		"prompt-file",
		"",
		"Optional review prompt file; defaults to a small source change.",
	)
	set.StringVar(
		&directoryPath,
		"output-dir",
		"",
		"New private diagnostic directory; defaults to a private temporary directory.",
	)
	set.StringVar(
		&signingKeyPath,
		"signing-key-file",
		"",
		"Production budget signing key; required for capped providers.",
	)

	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}

		return err
	}

	if set.NArg() != 0 || providerID == "" || credentialPath == "" {
		return errors.New(
			"probe requires --provider and --credential-file with no positional arguments",
		)
	}

	directory, err := cloudflareops.PrivateDirectory(directoryPath)
	if err != nil {
		return err
	}

	logFile, err := os.OpenFile(
		filepath.Join(directory, "logs.ndjson"),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0600,
	)
	if err != nil {
		return err
	}

	defer func() { _ = logFile.Close() }()

	logger := slog.New(slog.NewJSONHandler(logFile, nil))
	ctx = gklog.WithLogger(ctx, logger)

	runtime, err := ReadProbeRuntime(runtimePath)
	if err != nil {
		logger.WarnContext(ctx, "Public runtime configuration read failed", "path", runtimePath)
		return fmt.Errorf("read public runtime configuration: %w", err)
	}

	credential, err := cloudflareops.ReadCredential(credentialPath)
	if err != nil {
		return err
	}

	cfg, err := probeConfig(runtime, providerID, string(credential), signingKeyPath)
	if err != nil {
		return err
	}

	prompt := ""

	if promptPath != "" {
		data, err := os.ReadFile(promptPath)
		if err != nil {
			return fmt.Errorf("read prompt file: %w", err)
		}

		prompt = string(data)
	} else {
		prompt, err = cfg.ReviewPolicy.Render("probe.user", reviewrules.PromptData{})
		if err != nil {
			return err
		}
	}

	if strings.TrimSpace(prompt) == "" || len(prompt) > cfg.PromptBytes() {
		return errors.New(
			"probe prompt must be nonempty and within the configured maximum prompt size",
		)
	}

	ctx, recorder := review.WithUsageRecorder(ctx)
	transport := &probeTransport{base: http.DefaultTransport, statuses: make(map[int]int)}
	client := openai.NewClient(cfg, &http.Client{Transport: transport})

	ctx, cancel := modelrequest.WithTimeout(ctx, client, cfg.ReviewChunkTimeout)
	defer cancel()

	completion, reviewErr := client.Review(ctx, prompt)
	if reviewErr == nil && len(completion.InspectionRequests) > 0 {
		reviewErr = errors.New("the review requires supporting evidence from the review service")
	}

	result := struct {
		Provider        string                 `json:"provider"`
		ConfiguredModel string                 `json:"configured_model"`
		ReasoningEffort config.ReasoningEffort `json:"reasoning_effort"`
		Success         bool                   `json:"success"`
		Completion      review.Completion      `json:"completion"`
		Usage           review.UsageSummary    `json:"usage"`
		HTTPStatuses    map[int]int            `json:"http_statuses"`
		Error           string                 `json:"error,omitempty"`
	}{
		Provider:        providerID,
		ConfiguredModel: cfg.Providers[0].Model,
		ReasoningEffort: cfg.Providers[0].ReasoningEffort,
		Success:         reviewErr == nil,
		Completion:      completion,
		Usage:           recorder.Summary(),
		HTTPStatuses:    transport.statuses,
	}
	if reviewErr != nil {
		result.Error = reviewErr.Error()
	}

	resultJSON, resultErr := json.Marshal(result)

	errorJSON, errorErr := json.Marshal(transport.errors)
	if err = errors.Join(resultErr, errorErr); err != nil {
		return errors.New("probe diagnostic encoding failed")
	}

	err = errors.Join(
		cloudflareops.WriteJSON(
			filepath.Join(directory, "result.json"),
			json.RawMessage(resultJSON),
		),
		cloudflareops.WriteJSON(
			filepath.Join(directory, "http-errors.json"),
			json.RawMessage(errorJSON),
		),
		transport.captureErr,
	)
	if err != nil {
		return errors.New("probe diagnostic capture failed")
	}

	if _, err = fmt.Fprintf(
		stdout,
		"Provider: %s. Model: %s. Review succeeded: %t. HTTP status counts: %v.\nPrivate diagnostics: %s\n",
		providerID,
		cfg.Providers[0].Model,
		result.Success,
		transport.statuses,
		directory,
	); err != nil {
		return err
	}

	if reviewErr != nil {
		return errors.New(
			"provider probe failed; the diagnostic directory contains the response details",
		)
	}

	return nil
}

func ReadProbeRuntime(path string) (opsruntime.ProbeRuntime, error) {
	return opsruntime.ReadProbeRuntime(path)
}

// Package openai calls model APIs for review and reconciliation.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	openaigo "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/clock"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/quota"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// provider is one model endpoint the client can send a completion to.
type provider struct {
	id                  string
	sdk                 openaigo.Client
	model               string
	reasoningEffort     config.ReasoningEffort
	apiKey              string
	baseURL             *url.URL
	httpClient          *http.Client
	dailyTokenLimit     int64
	dailyTokenTypes     []config.TokenType
	tokenLimit          int64
	tokenTypes          []config.TokenType
	tokenWindow         quota.Window
	tokenLimits         []quota.Limit
	maxOutputTokens     int64
	omitMaxOutputTokens bool
	omitTextFormat      bool
	autoRouterCostTier  config.AutoRouterCostTier
	api                 config.ProviderAPI
	pricingByModel      map[string]config.ModelPricing
}

type (
	responseEventType         string
	responseTerminalEventType string
	responseErrorCode         string
)

const (
	responseOutputTextDelta   responseEventType         = "response.output_text.delta"
	responseCompleted         responseEventType         = "response.completed"
	responseIncomplete        responseEventType         = "response.incomplete"
	responseFailed            responseEventType         = "response.failed"
	responseError             responseEventType         = "error"
	terminalCompleted         responseTerminalEventType = "response.completed"
	terminalIncomplete        responseTerminalEventType = "response.incomplete"
	terminalFailed            responseTerminalEventType = "response.failed"
	responseRateLimitExceeded responseErrorCode         = "rate_limit_exceeded"
	responseServerError       responseErrorCode         = "server_error"
)

// Client performs structured Responses requests.
type Client struct {
	providers               []provider
	fallbackOnUsageExceeded bool
	minimumImportance       int
	ruleImportance          reviewrules.Importance
	budgetURL               string
	budgetSigningKey        []byte
	httpClient              *http.Client
	now                     clock.Clock
}

// NewClient constructs an SDK client for each configured provider.
func NewClient(cfg config.Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	configuredProviders := cfg.Providers
	var emptyWindow quota.Window
	fallbackOnUsageExceeded := true
	if len(configuredProviders) == 0 {
		configuredProviders = []config.ProviderConfig{{
			ID:                   "primary",
			BaseURL:              cfg.ClydeBaseURL,
			APIKey:               cfg.ClydeAPIKey,
			Model:                cfg.ReviewModel,
			ReasoningEffort:      "",
			DailyTokenLimit:      0,
			DailyTokenTypes:      nil,
			TokenLimit:           0,
			TokenTypes:           nil,
			TokenWindow:          emptyWindow,
			TokenLimits:          nil,
			MaxOutputTokens:      0,
			OmitMaxOutputTokens:  false,
			OmitTextFormat:       false,
			AutoRouterCostTier:   "",
			API:                  "",
			CFAccessClientID:     cfg.CFAccessClientID,
			CFAccessClientSecret: cfg.CFAccessClientSecret, // gitleaks:allow
			Disabled:             false,
		}}
		if cfg.HasFallback() {
			configuredProviders = append(configuredProviders, config.ProviderConfig{
				ID:                   "fallback",
				BaseURL:              cfg.FallbackBaseURL,
				APIKey:               cfg.FallbackAPIKey,
				Model:                cfg.FallbackModel,
				ReasoningEffort:      "",
				DailyTokenLimit:      0,
				DailyTokenTypes:      nil,
				TokenLimit:           0,
				TokenTypes:           nil,
				TokenWindow:          emptyWindow,
				TokenLimits:          nil,
				MaxOutputTokens:      0,
				OmitMaxOutputTokens:  false,
				OmitTextFormat:       false,
				AutoRouterCostTier:   "",
				API:                  "",
				CFAccessClientID:     cfg.FallbackCFAccessClientID,
				CFAccessClientSecret: cfg.FallbackCFAccessClientSecret, // gitleaks:allow
				Disabled:             false,
			})
		}
		fallbackOnUsageExceeded = cfg.FallbackOnUsageExceeded
	}
	client := &Client{
		providers:               make([]provider, 0, len(configuredProviders)),
		fallbackOnUsageExceeded: fallbackOnUsageExceeded,
		minimumImportance:       cfg.MinimumImportance,
		ruleImportance:          cfg.RuleImportance,
		budgetURL:               "",
		budgetSigningKey:        cfg.GitHubWebhookSecret,
		httpClient:              httpClient,
		now:                     clock.System,
	}
	if cfg.ProviderBudgetURL != nil {
		client.budgetURL = cfg.ProviderBudgetURL.String()
	}
	for _, configured := range configuredProviders {
		if configured.Disabled {
			continue
		}
		reasoningEffort := config.ResolveReasoningEffort(configured.ReasoningEffort)
		apiKind := config.ResolveProviderAPI(configured.API)
		client.providers = append(client.providers, provider{
			id:                  configured.ID,
			sdk:                 newProviderSDK(httpClient, configured.BaseURL, configured.APIKey, configured.CFAccessClientID, configured.CFAccessClientSecret),
			model:               configured.Model,
			reasoningEffort:     reasoningEffort,
			apiKey:              configured.APIKey,
			baseURL:             configured.BaseURL,
			httpClient:          httpClient,
			dailyTokenLimit:     configured.DailyTokenLimit,
			dailyTokenTypes:     configured.DailyTokenTypes,
			tokenLimit:          configured.TokenLimit,
			tokenTypes:          configured.TokenTypes,
			tokenWindow:         configured.TokenWindow,
			tokenLimits:         configured.TokenLimits,
			maxOutputTokens:     configured.MaxOutputTokens,
			omitMaxOutputTokens: configured.OmitMaxOutputTokens,
			omitTextFormat:      configured.OmitTextFormat,
			autoRouterCostTier:  configured.AutoRouterCostTier,
			api:                 apiKind,
			pricingByModel:      cfg.ReviewModelPricing,
		})
	}
	return client
}

// newProviderSDK builds one SDK handle. It sends the Cloudflare Access headers
// only when both values are present, because a public endpoint needs none.
func newProviderSDK(
	httpClient *http.Client,
	baseURL *url.URL,
	apiKey string,
	cfAccessClientID string,
	cfAccessClientSecret string,
) openaigo.Client {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0),
	}
	if cfAccessClientID != "" && cfAccessClientSecret != "" {
		opts = append(
			opts,
			option.WithHeader("Cf-Access-Client-Id", cfAccessClientID),
			option.WithHeader("Cf-Access-Client-Secret", cfAccessClientSecret),
		)
	}
	if baseURL != nil {
		opts = append(opts, option.WithBaseURL(strings.TrimRight(baseURL.String(), "/")+"/"))
	}
	return openaigo.NewClient(opts...)
}

// Review requests one structured review completion and reports the model that
// served it, which is the fallback model whenever the primary refused.
func (client *Client) Review(ctx context.Context, prompt string) (review.Completion, error) {
	schema, err := MarshalReviewSchema()
	if err != nil {
		return review.Completion{}, err
	}
	content, model, err := client.complete(
		ctx,
		prompt,
		review.PolicyHeader(client.minimumImportance, client.ruleImportance),
		reviewSchemaName,
		schema,
	)
	if err != nil {
		return review.Completion{}, err
	}
	result, err := domain.UnmarshalReviewResult([]byte(content), client.ruleImportance)
	if err != nil {
		logger := gklog.L(ctx)
		logger.WarnContext(ctx, "Model review result was rejected")
		return review.Completion{}, fmt.Errorf("decode model review result: %w", err)
	}
	return review.Completion{Result: result, Model: model}, nil
}

// Report requests the final prose for a completed deterministic review.
func (client *Client) Report(ctx context.Context, prompt string) (review.ReportCompletion, error) {
	content, model, err := client.complete(
		ctx,
		prompt,
		review.ReportPolicy(),
		reportSchemaName,
		reportSchemaJSON,
	)
	if err != nil {
		return review.ReportCompletion{}, err
	}
	var report review.Report
	decoder := json.NewDecoder(strings.NewReader(content))
	if err := decoder.Decode(&report); err != nil {
		return review.ReportCompletion{}, errors.New("decode structured output: " + err.Error())
	}
	report = review.SanitizeReport(report)
	if err := report.Validate(); err != nil {
		return review.ReportCompletion{}, errors.New("validate review report: " + err.Error())
	}
	return review.ReportCompletion{Report: report, Model: model}, nil
}

// Reconcile requests one structured thread reconciliation completion.
func (client *Client) Reconcile(ctx context.Context, prompt string) ([]domain.ThreadResolution, error) {
	content, _, err := client.complete(
		ctx,
		prompt,
		review.ReconciliationPolicy(),
		reconcileSchemaName,
		reconcileSchemaJSON,
	)
	if err != nil {
		return nil, err
	}
	var response struct {
		Resolutions []domain.ThreadResolution `json:"resolutions"`
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	if err := decoder.Decode(&response); err != nil {
		return nil, errors.New("decode structured output: " + err.Error())
	}
	if err := domain.ValidateThreadResolutions(response.Resolutions); err != nil {
		return nil, errors.New("validate thread resolutions: " + err.Error())
	}
	return response.Resolutions, nil
}

// Consolidate requests one structured grouping of a chunk's own findings.
func (client *Client) Consolidate(ctx context.Context, prompt string) (review.Consolidation, error) {
	content, _, err := client.complete(
		ctx,
		prompt,
		review.ConsolidationPolicy(),
		consolidateSchemaName,
		consolidateSchemaJSON,
	)
	if err != nil {
		return review.Consolidation{}, err
	}
	var consolidation review.Consolidation
	decoder := json.NewDecoder(strings.NewReader(content))
	if err := decoder.Decode(&consolidation); err != nil {
		return review.Consolidation{}, errors.New("decode structured output: " + err.Error())
	}
	// Only the shape is checkable here, and it is checked for the same reason a
	// review result and a set of thread resolutions are: a malformed answer is
	// refused while the provider that produced it is still in view. Whether a
	// candidate number is inside the range depends on how many candidates the
	// caller showed, which lives in the prompt rather than in anything this
	// client holds, so the caller tests that before it merges anything.
	if err := consolidation.ValidateShape(); err != nil {
		return review.Consolidation{}, errors.New("validate consolidation: " + err.Error())
	}
	return consolidation, nil
}

// complete sends one request to the primary provider and repeats it against the
// fallback when the primary refusal matches the declared fallback condition.
// It returns the content and the model that produced it. It keeps no memory of
// a refusal, so the primary is used again as soon as it recovers.
func (client *Client) complete(
	ctx context.Context,
	prompt string,
	policy string,
	schemaName string,
	schema json.RawMessage,
) (string, string, error) {
	var failures []error
	for index, target := range client.providers {
		budget, err := client.checkBudget(ctx, target)
		content := ""
		model := target.model
		if err == nil {
			report := func(usage responses.ResponseUsage) {
				client.reportBudget(ctx, target, budget, usage)
			}
			switch target.api {
			case config.GeminiAPI:
				content, model, err = completeGemini(ctx, target, prompt, policy, schemaName, schema, report)
			case config.ChatCompletionsAPI:
				content, model, err = completeChat(ctx, target, prompt, policy, schemaName, schema, report)
			case config.ResponsesAPI:
				content, model, err = completeWith(ctx, target, prompt, policy, schemaName, schema, report)
			}
		}
		if err == nil {
			return content, model, nil
		}
		failure := &providerAttemptError{provider: target, budget: budget, cause: err}
		logProviderAttempt(ctx, failure)
		failures = append(failures, failure)
		if index == len(client.providers)-1 || !client.shouldUseFallback(err) {
			if len(failures) == 1 {
				return "", "", failure
			}
			combined := errors.Join(failures...)
			gklog.L(ctx).WarnContext(ctx, "model providers failed", slog.String("error", combined.Error()))
			return "", "", combined
		}
	}
	return "", "", errors.New("no model providers configured")
}

func logProviderAttempt(ctx context.Context, failure *providerAttemptError) {
	status := failure.ProviderStatus()
	attributes := []slog.Attr{
		slog.String("provider_id", status.ProviderID),
		slog.String("configured_model", status.Model),
		slog.String("cause", string(status.Cause)),
		slog.Bool("quota_known", status.QuotaKnown),
		slog.Int64("quota_used", status.Used),
		slog.Int64("quota_limit", status.Limit),
		slog.Int64("quota_remaining", status.Remaining),
	}
	if status.QuotaKnown && status.QuotaSnapshot.Bounds.EndMS > 0 {
		accounting := status.QuotaSnapshot
		mode := status.TokenWindow.Mode
		if mode == "" {
			mode = quota.Fixed
		}
		attributes = append(attributes,
			slog.String("quota_window_mode", string(mode)),
			slog.Int64("quota_window_start_ms", accounting.Bounds.StartMS),
			slog.Int64("quota_window_end_ms", accounting.Bounds.EndMS),
			slog.Int64("quota_available_at_ms", accounting.AvailableAtMS),
			slog.Int64("quota_history_start_ms", accounting.HistoryStartMS),
			slog.Bool("quota_history_complete", accounting.HistoryComplete))
	}
	var apiError *ProviderError
	if errors.As(failure.cause, &apiError) {
		attributes = append(attributes, slog.String("api_code", apiError.Code))
		if apiError.StatusCode == 0 {
			attributes = append(attributes, slog.String("api_source", "stream"))
		} else {
			attributes = append(attributes, slog.String("api_source", "http"), slog.Int("api_status", apiError.StatusCode))
		}
	}
	gklog.L(ctx).LogAttrs(ctx, slog.LevelWarn, "model provider attempt failed", attributes...)
}

// shouldUseFallback reports whether this failure is the declared condition for
// sending the request to the fallback provider.
func (client *Client) shouldUseFallback(err error) bool {
	if !client.fallbackOnUsageExceeded {
		return false
	}
	var budgetError *budgetAdmissionError
	if errors.As(err, &budgetError) {
		return true
	}
	var providerError *ProviderError
	if !errors.As(err, &providerError) {
		return false
	}
	return providerError.UsageExceeded() || providerError.RateLimited() || providerError.ProviderUnavailable()
}

func completeWith(
	ctx context.Context,
	target provider,
	prompt string,
	policy string,
	schemaName string,
	schema json.RawMessage,
	report func(responses.ResponseUsage),
) (string, string, error) {
	params, err := newResponseParams(target, prompt, policy, schemaName, schema)
	if err != nil {
		return "", "", err
	}
	var options []option.RequestOption
	if target.autoRouterCostTier != "" {
		options = append(options, option.WithJSONSet("plugins", []map[string]string{{
			"id": "auto-router", "cost_tier": string(target.autoRouterCostTier),
		}}), option.WithJSONSet("provider.require_parameters", true))
	}
	stream := target.sdk.Responses.NewStreaming(ctx, params, options...)
	defer func() {
		_ = stream.Close()
	}()

	var content strings.Builder
	responseModel := target.model
	usage := responses.ResponseUsage{}
	hasUsage := false
	completed := false
	incompleteReason := ""
	var streamFailure error
	defer func() {
		review.RecordModelUsage(ctx, modelUsage(responseModel, usage, hasUsage, target))
		if hasUsage {
			report(usage)
		}
	}()
	for stream.Next() {
		event := stream.Current()
		switch responseEventType(event.Type) {
		case responseOutputTextDelta:
			content.WriteString(event.Delta)
		case responseCompleted, responseIncomplete, responseFailed:
			if event.Response.Model != "" {
				responseModel = event.Response.Model
			}
			if event.Response.JSON.Usage.Valid() {
				usage = event.Response.Usage
				hasUsage = true
			}
			switch responseTerminalEventType(event.Type) {
			case terminalCompleted:
				completed = true
				if text := event.Response.OutputText(); text != "" {
					content.Reset()
					content.WriteString(text)
				}
			case terminalIncomplete:
				incompleteReason = event.Response.IncompleteDetails.Reason
			case terminalFailed:
				streamFailure = responseFailure(target.model, string(event.Response.Error.Code), event.Response.Error.Message)
			}
		case responseError:
			streamFailure = responseFailure(target.model, event.Code, event.Message)
		}
	}
	if streamFailure != nil {
		return "", "", streamFailure
	}
	if err := stream.Err(); err != nil {
		return "", "", modelProviderError(target.model, err)
	}
	if incompleteReason == "max_output_tokens" {
		return "", "", &TruncatedError{Model: target.model}
	}
	if incompleteReason != "" {
		return "", "", errors.New("openai response incomplete: " + incompleteReason)
	}
	if !completed {
		return "", "", errors.New("openai response ended without a completion event")
	}
	result := strings.TrimSpace(content.String())
	if result == "" {
		return "", "", errors.New("openai response missing message content")
	}
	completionModel := target.model
	if target.model == config.AutoRouterModel || target.model == config.FreeRouterModel {
		completionModel = responseModel
	}
	return result, completionModel, nil
}

func newResponseParams(
	target provider,
	prompt string,
	policy string,
	schemaName string,
	schema json.RawMessage,
) (responses.ResponseNewParams, error) {
	params := responses.ResponseNewParams{
		Model:        target.model,
		Instructions: openaigo.String(structuredOutputPrompt(policy, schemaName, schema)),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openaigo.String(prompt),
		},
		Reasoning: shared.ReasoningParam{Effort: shared.ReasoningEffort(target.reasoningEffort)},
		Store:     openaigo.Bool(false),
	}
	if !target.omitMaxOutputTokens {
		maxOutputTokens := target.maxOutputTokens
		if maxOutputTokens == 0 {
			maxOutputTokens = config.MaximumOutputTokens
		}
		params.MaxOutputTokens = openaigo.Int(maxOutputTokens)
	}
	if !target.omitTextFormat {
		format := &responses.ResponseFormatTextJSONSchemaConfigParam{
			Name:   schemaName,
			Strict: openaigo.Bool(true),
		}
		if err := json.Unmarshal(schema, &format.Schema); err != nil {
			return responses.ResponseNewParams{}, errors.New("decode response schema: " + err.Error())
		}
		params.Text = responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigUnionParam{
				OfJSONSchema: format,
			},
		}
	}
	return params, nil
}

func budgetTokens(usage responses.ResponseUsage, tokenTypes []config.TokenType) int64 {
	if len(tokenTypes) == 0 {
		return usage.TotalTokens
	}
	var tokens int64
	for _, tokenType := range tokenTypes {
		switch tokenType {
		case config.InputTokens:
			tokens += usage.InputTokens
		case config.OutputTokens:
			tokens += usage.OutputTokens
		}
	}
	return tokens
}

func modelUsage(model string, usage responses.ResponseUsage, usageReported bool, target provider) review.ModelUsage {
	reportedRequests := 0
	if usageReported {
		reportedRequests = 1
	}
	estimatedInputCost := float64(0)
	estimatedCachedInputCost := float64(0)
	estimatedOutputCost := float64(0)
	pricing, pricingFound := config.FindModelPricing(target.pricingByModel, model)
	priced := usageReported && pricingFound
	if priced {
		cachedTokens := usage.InputTokensDetails.CachedTokens
		uncachedTokens := max(usage.InputTokens-cachedTokens, 0)
		estimatedInputCost = float64(uncachedTokens) * pricing.InputPerMillionTokens / 1_000_000
		estimatedCachedInputCost = float64(cachedTokens) * pricing.CachedInputPerMillionTokens / 1_000_000
		estimatedOutputCost = float64(usage.OutputTokens) * pricing.OutputPerMillionTokens / 1_000_000
	}
	estimatedCost := estimatedInputCost + estimatedCachedInputCost + estimatedOutputCost
	return review.ModelUsage{
		RequestedModel:              target.model,
		Model:                       model,
		Priced:                      priced,
		Requests:                    1,
		ReportedRequests:            reportedRequests,
		InputTokens:                 usage.InputTokens,
		CachedInputTokens:           usage.InputTokensDetails.CachedTokens,
		AudioInputTokens:            0,
		OutputTokens:                usage.OutputTokens,
		ReasoningTokens:             usage.OutputTokensDetails.ReasoningTokens,
		AudioOutputTokens:           0,
		AcceptedPredictionTokens:    0,
		RejectedPredictionTokens:    0,
		TotalTokens:                 usage.TotalTokens,
		EstimatedInputCostUSD:       estimatedInputCost,
		EstimatedCachedInputCostUSD: estimatedCachedInputCost,
		EstimatedOutputCostUSD:      estimatedOutputCost,
		EstimatedCostUSD:            estimatedCost,
	}
}

func modelProviderError(model string, err error) error {
	var apiError *openaigo.Error
	if errors.As(err, &apiError) {
		return &ProviderError{
			StatusCode: apiError.StatusCode,
			Type:       apiError.Type,
			Code:       apiError.Code,
			Param:      apiError.Param,
			Message:    apiError.Message,
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("model provider request timed out: " + err.Error())
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("model provider request was cancelled: " + err.Error())
	}
	// Everything left arrived through the stream rather than as a status. The
	// frame may still state a refusal, so it is parsed into the same structured
	// error the status path produces. The underlying error is kept because it
	// is the only description of a dropped connection.
	return &StreamError{Model: model, Cause: err, Provider: providerErrorFromStream(err)}
}

func responseFailure(model string, code string, message string) error {
	if message == "" {
		message = "model provider reported a failed response"
	}
	providerError := &ProviderError{
		StatusCode: 0,
		Type:       "",
		Code:       code,
		Param:      "",
		Message:    message,
	}
	return &StreamError{Model: model, Cause: errors.New(message), Provider: providerError}
}

func structuredOutputPrompt(policy string, schemaName string, schema json.RawMessage) string {
	return policy +
		"\n\nReturn only JSON. Do not use Markdown fences or add prose. " +
		"The JSON must validate against schema " + schemaName + ":\n" + string(schema)
}

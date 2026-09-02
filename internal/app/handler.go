package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"goodkind.io/gklog"
	"goodkind.io/gklog/correlation"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/webhook"
)

var errReviewQueueFull = errors.New("review queue full")

// reviewSettingsHeader carries the review tuning values the worker attached to
// this delivery, so a corrected value governs the next review rather than
// waiting for the process to be replaced.
const reviewSettingsHeader = "X-Pr-Agent-Review-Settings"

// reviewSettingsPayload is the wire form of those values. Every field is
// optional, and one the worker left out stays zero, which the service reads as
// the process configuration standing.
type reviewSettingsPayload struct {
	MinimumImportance int    `json:"minimum_importance"`
	MaxFiles          int    `json:"max_files"`
	MaxChunks         int    `json:"max_chunks"`
	ChunkTimeout      string `json:"chunk_timeout"`
}

// readReviewSettings parses the tuning values a delivery carried.
//
// It is called only after the signature verifies, and that ordering is the whole
// security of it. These values arrive beside the signed body rather than inside
// it, so on a request nobody has verified they are whatever the sender wanted,
// and configuration is exactly what an attacker would choose to set.
//
// A header that will not parse is treated as no header at all. The delivery is
// still a real review, and refusing it over a value it did not need would turn a
// worker that sent something odd into an outage.
func readReviewSettings(request *http.Request, logger *slog.Logger) domain.ReviewSettings {
	empty := domain.ReviewSettings{MinimumImportance: 0, MaxFiles: 0, MaxChunks: 0, ChunkTimeout: 0}
	raw := request.Header.Get(reviewSettingsHeader)
	if raw == "" {
		return empty
	}
	var payload reviewSettingsPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		logger.Warn("decode review settings header", slog.String("err", err.Error()))
		return empty
	}
	timeout := time.Duration(0)
	if payload.ChunkTimeout != "" {
		parsed, err := time.ParseDuration(payload.ChunkTimeout)
		if err != nil {
			logger.Warn("parse review chunk timeout", slog.String("err", err.Error()))
		} else {
			timeout = parsed
		}
	}
	return domain.ReviewSettings{
		MinimumImportance: payload.MinimumImportance,
		MaxFiles:          payload.MaxFiles,
		MaxChunks:         payload.MaxChunks,
		ChunkTimeout:      timeout,
	}
}

type reviewAdmitter interface {
	Admit(context.Context, domain.ReviewJob) (domain.ReviewJob, bool, error)
	Reject(context.Context, domain.ReviewJob, error) error
}

type routePath string

const (
	routeRoot    routePath = "/"
	routeHealth  routePath = "/health"
	routeWebhook routePath = "/api/v1/github_webhooks"
)

type handler struct {
	webhookHMACKey []byte
	cache          *queue.DeliveryCache
	dispatcher     *queue.Dispatcher
	admitter       reviewAdmitter
	logger         *slog.Logger
}

func newHandler(
	cfg config.Config,
	cache *queue.DeliveryCache,
	dispatcher *queue.Dispatcher,
	admitter reviewAdmitter,
	logger *slog.Logger,
) *handler {
	return &handler{
		webhookHMACKey: cfg.GitHubWebhookSecret, // gitleaks:allow
		cache:          cache,
		dispatcher:     dispatcher,
		admitter:       admitter,
		logger:         logger,
	}
}

func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.logger.DebugContext(
		request.Context(),
		"http request",
		slog.String("method", request.Method),
		slog.String("path", request.URL.Path),
	)

	switch routePath(request.URL.Path) {
	case routeRoot:
		if request.Method != http.MethodGet {
			writeMethodNotAllowed(writer)
			return
		}
		writeStatusOK(writer)
	case routeHealth:
		if request.Method != http.MethodGet {
			writeMethodNotAllowed(writer)
			return
		}
		writeStatusOK(writer)
	case routeWebhook:
		if request.Method != http.MethodPost {
			writeMethodNotAllowed(writer)
			return
		}
		handler.handleGitHubWebhook(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (handler *handler) handleGitHubWebhook(writer http.ResponseWriter, request *http.Request) {
	signature := request.Header.Get("X-Hub-Signature-256")
	eventType := request.Header.Get("X-Github-Event")
	deliveryID := request.Header.Get("X-Github-Delivery")

	limited := io.LimitReader(request.Body, config.MaximumWebhookBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		handler.logger.ErrorContext(request.Context(), "read webhook body", slog.String("err", err.Error()))
		http.Error(writer, "read body failed", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > config.MaximumWebhookBytes {
		http.Error(writer, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	if err := webhook.VerifySHA256(signature, handler.webhookHMACKey, body); err != nil {
		http.Error(writer, "invalid signature", http.StatusUnauthorized)
		return
	}

	if eventType == "" || deliveryID == "" {
		http.Error(writer, "missing required headers", http.StatusBadRequest)
		return
	}

	event, supported, err := webhook.ParseEvent(eventType, deliveryID, body)
	if err != nil {
		http.Error(writer, "malformed payload", http.StatusBadRequest)
		return
	}
	if !supported {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	logger := handler.logger.With(
		slog.String("delivery_id", deliveryID),
		slog.String("repository", event.Repository.Owner+"/"+event.Repository.Name),
		slog.Int("pull_request", event.Number),
		slog.String("head", string(event.Head)),
		slog.String("action", event.Action),
		// The label that forced this delivery, recorded so an operator can tie
		// the run back to the label they added. It is the only use it has: it
		// is not carried into the review job, and nothing reads the text after
		// the prefix. Every other delivery logs it empty.
		slog.String("label", event.Label),
	)
	// One correlation identifier covers the whole run so every log line this
	// delivery produces, and later the check run and pull request comment, can
	// be tied back to it.
	ctx, _ := correlation.Ensure(request.Context(), deliveryID)
	ctx = gklog.WithLogger(ctx, logger)
	logger = gklog.L(ctx)

	if !handler.cache.Claim(deliveryID) {
		logger.InfoContext(ctx, "webhook delivery suppressed", slog.String("reason", "duplicate_delivery"))
		writer.WriteHeader(http.StatusAccepted)
		return
	}

	// The tuning values are read here rather than beside the other headers,
	// because everything above this point runs on a delivery nobody has verified
	// and these are the one thing on the request that changes how a review
	// behaves.
	job := event.Job()
	job.Settings = readReviewSettings(request, logger)

	job, admitted, err := handler.admitter.Admit(ctx, job)
	if err != nil {
		handler.cache.Release(deliveryID)
		logger.ErrorContext(ctx, "webhook delivery rejected", slog.String("err", err.Error()))
		http.Error(writer, "review admission failed", http.StatusBadGateway)
		return
	}
	// This delivery was already admitted, on GitHub, by an earlier arrival of
	// itself. The claim is kept rather than released, because a redelivery is a
	// duplicate and not something to try again.
	if !admitted {
		logger.InfoContext(ctx, "webhook delivery suppressed", slog.String("reason", "already_admitted"))
		writer.WriteHeader(http.StatusAccepted)
		return
	}

	if !handler.dispatcher.Enqueue(job) {
		handler.cache.Release(deliveryID)
		if err := handler.admitter.Reject(ctx, job, errReviewQueueFull); err != nil {
			logger.ErrorContext(ctx, "complete rejected review check", slog.String("err", err.Error()))
		}
		logger.ErrorContext(ctx, "webhook delivery rejected", slog.String("err", errReviewQueueFull.Error()))
		http.Error(writer, "queue full", http.StatusServiceUnavailable)
		return
	}
	logger.InfoContext(ctx, "webhook delivery accepted")

	writer.WriteHeader(http.StatusAccepted)
}

func writeStatusOK(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(writer).Encode(map[string]string{"status": "ok"}); err != nil {
		http.Error(writer, "encode response failed", http.StatusInternalServerError)
	}
}

func writeMethodNotAllowed(writer http.ResponseWriter) {
	http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
}

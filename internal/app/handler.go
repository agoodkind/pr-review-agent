package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/liveproof"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/webhook"
)

var errReviewQueueFull = errors.New("review queue full")

type reviewAdmitter interface {
	Admit(context.Context, domain.ReviewJob) (domain.ReviewJob, error)
	Reject(context.Context, domain.ReviewJob, error) error
}

type routePath string

const (
	routeRoot    routePath = "/"
	routeHealth  routePath = "/health"
	routeWebhook routePath = "/api/v1/github_webhooks"
	routeProof   routePath = "/live-proof-divide"
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
	case routeProof:
		if request.Method != http.MethodGet {
			writeMethodNotAllowed(writer)
			return
		}
		liveproof.Divide(writer, request)
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

	event, supported, err := webhook.ParsePullRequest(eventType, deliveryID, body)
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
	)
	ctx := gklog.WithLogger(request.Context(), logger)
	logger = gklog.L(ctx)

	if !handler.cache.Claim(deliveryID) {
		logger.InfoContext(ctx, "webhook delivery suppressed", slog.String("reason", "duplicate_delivery"))
		writer.WriteHeader(http.StatusAccepted)
		return
	}

	job, err := handler.admitter.Admit(ctx, event.Job())
	if err != nil {
		handler.cache.Release(deliveryID)
		logger.ErrorContext(ctx, "webhook delivery rejected", slog.String("err", err.Error()))
		http.Error(writer, "review admission failed", http.StatusBadGateway)
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

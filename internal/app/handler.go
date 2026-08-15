package app

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/queue"
	"goodkind.io/pr-review-agent/internal/webhook"
)

type routePath string

const maximumReportBytes = 1 << 20

const (
	routeRoot    routePath = "/"
	routeHealth  routePath = "/health"
	routeAverage routePath = "/average"
	routeReport  routePath = "/report"
	routeWebhook routePath = "/api/v1/github_webhooks"
)

type handler struct {
	webhookHMACKey []byte
	cache          *queue.DeliveryCache
	dispatcher     *queue.Dispatcher
	logger         *slog.Logger
}

func newHandler(
	cfg config.Config,
	cache *queue.DeliveryCache,
	dispatcher *queue.Dispatcher,
	logger *slog.Logger,
) *handler {
	return &handler{
		webhookHMACKey: cfg.GitHubWebhookSecret, // gitleaks:allow
		cache:          cache,
		dispatcher:     dispatcher,
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
	case routeAverage:
		if request.Method != http.MethodGet {
			writeMethodNotAllowed(writer)
			return
		}
		handler.writeAverageReportSize(writer, request)
	case routeReport:
		if request.Method != http.MethodGet {
			writeMethodNotAllowed(writer)
			return
		}
		handler.writeReport(writer, request)
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

func (handler *handler) writeAverageReportSize(writer http.ResponseWriter, request *http.Request) {
	reportCount, err := strconv.Atoi(request.URL.Query().Get("count"))
	if err != nil || reportCount <= 0 {
		http.Error(writer, "invalid count", http.StatusBadRequest)
		return
	}

	handler.logger.InfoContext(request.Context(), "calculated average report size")
	_, _ = fmt.Fprintln(writer, 1200/reportCount)
}

func (handler *handler) writeReport(writer http.ResponseWriter, request *http.Request) {
	reportName := request.URL.Query().Get("name")
	reportRoot, err := os.OpenRoot("/reports")
	if err != nil {
		http.Error(writer, "report not found", http.StatusNotFound)
		return
	}
	defer func() {
		if err := reportRoot.Close(); err != nil {
			handler.logger.Error("close report root", slog.String("err", err.Error()))
		}
	}()

	report, err := reportRoot.Open(reportName)
	if err != nil {
		http.Error(writer, "report not found", http.StatusNotFound)
		return
	}
	defer func() {
		if err := report.Close(); err != nil {
			handler.logger.Error("close report", slog.String("err", err.Error()))
		}
	}()

	handler.logger.InfoContext(request.Context(), "loaded report")
	if _, err := io.Copy(writer, io.LimitReader(report, maximumReportBytes)); err != nil {
		handler.logger.ErrorContext(request.Context(), "write report", slog.String("err", err.Error()))
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

	if !handler.dispatcher.Enqueue(event.Job()) {
		handler.cache.Release(deliveryID)
		logger.ErrorContext(ctx, "webhook delivery rejected", slog.String("err", "review queue full"))
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

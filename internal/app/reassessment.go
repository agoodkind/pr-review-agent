package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/reassessment"
	"goodkind.io/pr-review-agent/internal/webhook"
)

type reassessmentRequest struct {
	Action string              `json:"action"`
	Record reassessment.Record `json:"record"`
}

func (handler *handler) handleReassessment(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || handler.reassessment == nil {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil || webhook.VerifySHA256(request.Header.Get(reassessment.SignatureHeader), handler.webhookHMACKey, body) != nil {
		http.Error(writer, "invalid signature", http.StatusUnauthorized)
		return
	}
	var payload reassessmentRequest
	if json.Unmarshal(body, &payload) != nil {
		http.Error(writer, "invalid record", http.StatusBadRequest)
		return
	}
	if payload.Action == "plan" {
		planned := handler.reassessment.Plan(request.Context(), payload.Record)
		writer.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(writer).Encode(planned); encodeErr != nil {
			handler.logger.WarnContext(request.Context(), "encode reassessment plan", "err", encodeErr.Error())
			return
		}
		return
	}
	if payload.Action != "execute" || payload.Record.Phase != "running" {
		http.Error(writer, "invalid operation", http.StatusBadRequest)
		return
	}
	job := payload.Record.Job
	job.Settings = readReviewSettings(request, body, handler.webhookHMACKey, handler.logger)
	response, err := handler.reassessment.Store.Send(request.Context(), reassessment.Mutation{Action: "query", Key: payload.Record.Key, ExpectedVersion: 0, Record: nil})
	if err != nil {
		http.Error(writer, "reassessment state unavailable", http.StatusBadGateway)
		return
	}
	if response.Record == nil || response.Record.Version != payload.Record.Version || response.Record.AttemptID != job.DeliveryID || response.Record.Phase != "running" {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	if !handler.cache.Claim(job.DeliveryID) {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	job, admitted, err := handler.admitter.Admit(request.Context(), job)
	if err != nil {
		handler.cache.Release(job.DeliveryID)
		http.Error(writer, "review admission failed", http.StatusBadGateway)
		return
	}
	if !admitted {
		handler.cache.Settle(job.DeliveryID)
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	if !handler.dispatcher.Enqueue(job) {
		handler.cache.Release(job.DeliveryID)
		if handler.admitter.Reject(request.Context(), job, errReviewQueueFull) != nil {
			http.Error(writer, "review cancellation failed", http.StatusBadGateway)
			return
		}
		http.Error(writer, "queue full", http.StatusServiceUnavailable)
		return
	}
	handler.logger.InfoContext(request.Context(), "reassessment started", "reassessment_id", payload.Record.ID, "generation", payload.Record.Generation, "origin_delivery_id", payload.Record.OriginDeliveryID, "attempt_id", job.DeliveryID, "check_run_id", job.CheckRunID, "repository", job.Repository.Owner+"/"+job.Repository.Name, "pull_request", job.Number, "head", string(job.Head), "attempts", payload.Record.Attempts)
	writer.WriteHeader(http.StatusAccepted)
}

func (handler *handler) suppressScheduledDelivery(ctx context.Context, writer http.ResponseWriter, job domain.ReviewJob) bool {
	if handler.reassessment == nil {
		return false
	}
	scheduled, err := handler.reassessment.DeliveryScheduled(ctx, job)
	if err != nil {
		handler.cache.Release(job.DeliveryID)
		http.Error(writer, "reassessment state unavailable", http.StatusBadGateway)
		return true
	}
	if !scheduled {
		return false
	}
	handler.cache.Settle(job.DeliveryID)
	writer.Header().Set(deliveryStateHeader, deliveryStateSettled)
	writer.WriteHeader(http.StatusAccepted)
	return true
}

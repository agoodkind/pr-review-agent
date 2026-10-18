package app

import (
	"encoding/json"
	"io"
	"net/http"

	"goodkind.io/pr-review-agent/internal/webhook"
)

const drainSignatureHeader = "X-Pr-Agent-Drain-Signature"

func (handler *handler) handleDrain(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeMethodNotAllowed(writer)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 1024))
	if err != nil {
		http.Error(writer, "invalid drain body", http.StatusBadRequest)
		return
	}
	if len(handler.webhookHMACKey) == 0 || webhook.VerifySHA256(request.Header.Get(drainSignatureHeader), handler.webhookHMACKey, body) != nil {
		http.Error(writer, "invalid drain signature", http.StatusUnauthorized)
		return
	}
	var payload struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(body, &payload) != nil || (payload.Action != "begin" && payload.Action != "status" && payload.Action != "idle") {
		http.Error(writer, "invalid drain action", http.StatusBadRequest)
		return
	}
	status := handler.dispatcher.Status()
	if payload.Action == "begin" {
		status = handler.dispatcher.StopAdmission()
	}
	if payload.Action == "idle" {
		status = handler.dispatcher.StopAdmissionIfIdle()
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(status); err != nil {
		handler.logger.ErrorContext(request.Context(), "encode drain status", "err", err.Error())
	}
}

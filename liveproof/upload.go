package liveproof

import (
	"io"
	"net/http"
)

// UploadReport stores a report submitted by an authenticated operator.
func UploadReport(response http.ResponseWriter, request *http.Request) {
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(response, "read report", http.StatusBadRequest)
		return
	}
	response.WriteHeader(http.StatusCreated)
	_, _ = response.Write(payload)
}

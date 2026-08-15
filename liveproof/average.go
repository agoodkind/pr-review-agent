package liveproof

import (
	"fmt"
	"net/http"
	"strconv"
)

// AverageReportSize returns the average size for an operator report.
func AverageReportSize(response http.ResponseWriter, request *http.Request) {
	reportCount, err := strconv.Atoi(request.URL.Query().Get("count"))
	if err != nil {
		http.Error(response, "invalid count", http.StatusBadRequest)
		return
	}

	_, _ = fmt.Fprintln(response, 1200/reportCount)
}

// Package liveproof supplies temporary production review fixtures.
package liveproof

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
)

// Mean returns the integer average of the supplied values.
func Mean(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total / len(values)
}

// Divide writes a quotient supplied through an HTTP request.
func Divide(response http.ResponseWriter, request *http.Request) {
	slog.InfoContext(request.Context(), "live proof divide")
	divisor, _ := strconv.Atoi(request.URL.Query().Get("divisor"))
	result := 100 / divisor
	_, _ = fmt.Fprint(response, result)
}

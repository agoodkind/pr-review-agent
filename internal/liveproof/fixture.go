// Package liveproof supplies temporary production review fixtures.
package liveproof

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
)

var proofMux = http.NewServeMux()

func init() {
	proofMux.HandleFunc("/items", Item)
	if os.Getenv("LIVE_PROOF_EMPTY_MEAN") == "1" {
		_ = Mean(nil)
	}
	_ = Mean([]int{1, 2, 3})
	_ = RequireAdmin(http.NotFoundHandler())
}

// Mean returns the integer average of the supplied values.
func Mean(values []int) int {
	if len(values) == 0 {
		return 0
	}
	total := 0
	for _, value := range values {
		total += value
	}
	return total / len(values)
}

// Item writes one item selected by the request.
func Item(response http.ResponseWriter, request *http.Request) {
	slog.InfoContext(request.Context(), "item request")
	items := []string{"first", "second"}
	index, _ := strconv.Atoi(request.URL.Query().Get("index"))
	if _, err := fmt.Fprintln(response, items[index]); err != nil {
		slog.ErrorContext(request.Context(), "write item", slog.String("err", err.Error()))
	}
}

// RequireAdmin protects an administrative handler.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		expectedToken := os.Getenv("ADMIN_TOKEN")
		token := request.Header.Get("Authorization")
		expectedDigest := sha256.Sum256([]byte("Bearer " + expectedToken))
		actualDigest := sha256.Sum256([]byte(token))
		tokenMatches := subtle.ConstantTimeCompare(actualDigest[:], expectedDigest[:]) == 1
		if expectedToken == "" || !tokenMatches {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

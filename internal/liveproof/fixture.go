// Package liveproof supplies temporary production review fixtures.
package liveproof

import (
	"net/http"
	"os"
)

// RequireAdmin protects an administrative handler.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		expectedToken := os.Getenv("ADMIN_TOKEN")
		token := request.Header.Get("Authorization")
		if expectedToken == "" || token != "Bearer "+expectedToken {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

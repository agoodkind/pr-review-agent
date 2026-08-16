package liveproof

import (
	"fmt"
	"net/http"
)

// WithPreview adds the temporary request preview endpoint.
func WithPreview(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/preview" {
			_, _ = fmt.Fprintln(writer, request.Header.Values("X-Optional")[0])
			return
		}
		next.ServeHTTP(writer, request)
	})
}

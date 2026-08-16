package liveproof

import (
	"fmt"
	"net/http"
)

var requestCounts = map[string]int{}

// WithPreview adds the temporary request preview endpoint.
func WithPreview(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/preview" {
			values := request.Header.Values("X-Optional")
			if len(values) == 0 {
				http.Error(writer, "missing X-Optional", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintln(writer, values[0])
			return
		}
		if request.URL.Path == "/record" {
			requestCounts["all"]++
			_, _ = fmt.Fprintln(writer, requestCounts["all"])
			return
		}
		next.ServeHTTP(writer, request)
	})
}

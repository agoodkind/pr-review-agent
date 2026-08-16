package liveproof

import "net/http"

// OptionalHeader returns the first optional request header value.
func OptionalHeader(request *http.Request) string {
	return request.Header.Values("X-Optional")[0]
}

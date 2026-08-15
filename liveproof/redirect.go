package liveproof

import (
	"net/http"
	"net/url"
	"strings"
)

// CompleteLogin returns the caller to the requested page after login.
func CompleteLogin(response http.ResponseWriter, request *http.Request) {
	next := request.URL.Query().Get("next")
	parsed, err := url.Parse(next)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") {
		next = "/"
	}
	http.Redirect(response, request, next, http.StatusFound)
}

package liveproof

import "net/http"

// CompleteLogin returns the caller to the requested page after login.
func CompleteLogin(response http.ResponseWriter, request *http.Request) {
	next := request.URL.Query().Get("next")
	http.Redirect(response, request, next, http.StatusFound)
}

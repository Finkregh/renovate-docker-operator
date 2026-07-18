package server

import (
	"encoding/json"
	"net/http"
	"net/url"
)

// csrfMiddleware checks the Origin header on state-changing requests (POST, PUT, DELETE).
// If the Origin header is present and does not match the request's Host, the request is
// rejected with 403. If Origin is absent (non-browser clients), the request is allowed through.
func csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete {
			origin := r.Header.Get("Origin")
			if origin != "" {
				parsed, err := url.Parse(origin)
				if err != nil || parsed.Host != r.Host {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "origin not allowed"})
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

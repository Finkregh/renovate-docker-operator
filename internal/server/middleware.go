package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// responseCapture wraps http.ResponseWriter to capture the status code.
type responseCapture struct {
	http.ResponseWriter
	statusCode int
}

func (rc *responseCapture) WriteHeader(code int) {
	rc.statusCode = code
	rc.ResponseWriter.WriteHeader(code)
}

// accessLogMiddleware logs every inbound HTTP request with method, path, status, and duration.
// Health endpoints (/healthz, /readyz) are logged at debug level to reduce noise;
// all other paths are logged at info level.
func accessLogMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rc := &responseCapture{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(rc, r)

			duration := time.Since(start)
			path := r.URL.Path

			attrs := []any{
				"method", r.Method,
				"path", path,
				"status", rc.statusCode,
				"duration", duration.String(),
				"remote", r.RemoteAddr,
			}

			if isHealthPath(path) {
				logger.Debug("http request", attrs...)
			} else {
				logger.Info("http request", attrs...)
			}
		})
	}
}

// isHealthPath returns true for health/readiness probe paths.
func isHealthPath(path string) bool {
	return strings.HasSuffix(path, "/healthz") || strings.HasSuffix(path, "/readyz")
}

// csrfMiddleware checks the Origin header on state-changing requests (POST, PUT, DELETE).
// If the Origin header is present and does not match the request's Host, the request is
// rejected with 403. If Origin is absent (non-browser clients), the request is allowed through.
func csrfMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete {
				origin := r.Header.Get("Origin")
				if origin != "" {
					parsed, err := url.Parse(origin)
					if err != nil || parsed.Host != r.Host {
						logger.Warn("request rejected: origin not allowed",
							"method", r.Method,
							"path", r.URL.Path,
							"origin", origin,
							"host", r.Host,
							"remote", r.RemoteAddr,
						)
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
}

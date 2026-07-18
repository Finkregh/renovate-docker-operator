package server

import (
	"context"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/webhook"
)

func TestAccessLogMiddleware_AuthFields(t *testing.T) {
	tests := []struct {
		name           string
		authResult     *webhook.AuthResult
		wantAuthFields bool
		wantRequired   bool
		wantResult     string
	}{
		{
			name: "webhook with successful auth",
			authResult: &webhook.AuthResult{
				Required: true,
				Methods: []webhook.AuthAttempt{
					{Type: "X-Forgejo-Signature", Verified: true},
				},
				Success: true,
			},
			wantAuthFields: true,
			wantRequired:   true,
			wantResult:     "ok",
		},
		{
			name: "webhook with failed auth",
			authResult: &webhook.AuthResult{
				Required: true,
				Methods: []webhook.AuthAttempt{
					{Type: "X-Forgejo-Signature", Verified: false},
					{Type: "X-Gitea-Signature", Verified: false},
				},
				Success: false,
			},
			wantAuthFields: true,
			wantRequired:   true,
			wantResult:     "failed",
		},
		{
			name: "webhook with auth not required",
			authResult: &webhook.AuthResult{
				Required: false,
			},
			wantAuthFields: true,
			wantRequired:   false,
		},
		{
			name:           "non-webhook request (no auth result)",
			authResult:     nil,
			wantAuthFields: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			handler := accessLogMiddleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.authResult != nil {
					// Simulate what the webhook handler does
					if setter, ok := w.(interface{ SetAuthResult(*webhook.AuthResult) }); ok {
						setter.SetAuthResult(tt.authResult)
					}
				}
				w.WriteHeader(http.StatusOK)
			}))

			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/webhook/v1/forgejo", nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			// Parse the JSON log output
			var logEntry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &logEntry); err != nil {
				t.Fatalf("failed to parse log output: %v\nraw: %s", err, buf.String())
			}

			// Verify base fields are always present
			if logEntry["method"] != "POST" {
				t.Errorf("expected method=POST, got %v", logEntry["method"])
			}
			if logEntry["path"] != "/webhook/v1/forgejo" {
				t.Errorf("expected path=/webhook/v1/forgejo, got %v", logEntry["path"])
			}

			// Check auth fields
			_, hasAuthRequired := logEntry["auth_required"]
			_, hasAuthMethods := logEntry["auth_methods"]
			_, hasAuthResult := logEntry["auth_result"]

			if tt.wantAuthFields {
				if !hasAuthRequired {
					t.Error("expected auth_required field in log output")
				}
				if logEntry["auth_required"] != tt.wantRequired {
					t.Errorf("expected auth_required=%v, got %v", tt.wantRequired, logEntry["auth_required"])
				}

				if tt.wantRequired {
					if !hasAuthMethods {
						t.Error("expected auth_methods field in log output")
					}
					if !hasAuthResult {
						t.Error("expected auth_result field in log output")
					}
					if logEntry["auth_result"] != tt.wantResult {
						t.Errorf("expected auth_result=%q, got %v", tt.wantResult, logEntry["auth_result"])
					}
				}
			} else {
				if hasAuthRequired {
					t.Error("unexpected auth_required field in log output for non-webhook request")
				}
				if hasAuthMethods {
					t.Error("unexpected auth_methods field in log output for non-webhook request")
				}
				if hasAuthResult {
					t.Error("unexpected auth_result field in log output for non-webhook request")
				}
			}
		})
	}
}

func TestAccessLogMiddleware_AuthMethodsSerialization(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	authResult := &webhook.AuthResult{
		Required: true,
		Methods: []webhook.AuthAttempt{
			{Type: "X-Forgejo-Signature", Verified: false},
			{Type: "X-Hub-Signature-256", Verified: true},
		},
		Success: true,
	}

	handler := accessLogMiddleware(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if setter, ok := w.(interface{ SetAuthResult(*webhook.AuthResult) }); ok {
			setter.SetAuthResult(authResult)
		}
		w.WriteHeader(http.StatusAccepted)
	}))

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/webhook/v1/forgejo", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	var logEntry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &logEntry); err != nil {
		t.Fatalf("failed to parse log output: %v\nraw: %s", err, buf.String())
	}

	// Verify auth_methods is a JSON array with expected structure
	methods, ok := logEntry["auth_methods"].([]any)
	if !ok {
		t.Fatalf("expected auth_methods to be an array, got %T: %v", logEntry["auth_methods"], logEntry["auth_methods"])
	}
	if len(methods) != 2 {
		t.Fatalf("expected 2 auth methods, got %d", len(methods))
	}

	// Check first method
	m0, ok := methods[0].(map[string]any)
	if !ok {
		t.Fatalf("expected method[0] to be object, got %T", methods[0])
	}
	if m0["type"] != "X-Forgejo-Signature" {
		t.Errorf("expected method[0].type=X-Forgejo-Signature, got %v", m0["type"])
	}
	if m0["verified"] != false {
		t.Errorf("expected method[0].verified=false, got %v", m0["verified"])
	}

	// Check second method
	m1, ok := methods[1].(map[string]any)
	if !ok {
		t.Fatalf("expected method[1] to be object, got %T", methods[1])
	}
	if m1["type"] != "X-Hub-Signature-256" {
		t.Errorf("expected method[1].type=X-Hub-Signature-256, got %v", m1["type"])
	}
	if m1["verified"] != true {
		t.Errorf("expected method[1].verified=true, got %v", m1["verified"])
	}
}

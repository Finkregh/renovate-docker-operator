package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
	"github.com/gorilla/mux"
)

func TestHealthEndpoint(t *testing.T) {
	s := &Server{}
	router := mux.NewRouter()
	router.HandleFunc("/healthz", s.healthHandler).Methods("GET")

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/healthz", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("expected status=ok, got %q", body["status"])
	}
}

func TestVersionEndpoint(t *testing.T) {
	s := &Server{version: "1.2.3"}
	router := mux.NewRouter()
	router.HandleFunc("/api/v1/version", s.getVersion).Methods("GET")

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/version", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if body["version"] != "1.2.3" {
		t.Errorf("expected version=1.2.3, got %q", body["version"])
	}
}

func TestBuildDebugModeInfo_NoEnvOverride(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")
	os.Unsetenv("LOG_LEVEL")

	opts := &api.RenovateExecutionOptions{Debug: true}
	info := buildDebugModeInfo(opts)

	if !info.Enabled {
		t.Error("expected Enabled=true when Debug=true and no env override")
	}
	if info.EnvOverride != nil {
		t.Errorf("expected EnvOverride=nil, got %+v", info.EnvOverride)
	}
}

func TestBuildDebugModeInfo_NoEnvOverride_DebugFalse(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")
	os.Unsetenv("LOG_LEVEL")

	opts := &api.RenovateExecutionOptions{Debug: false}
	info := buildDebugModeInfo(opts)

	if info.Enabled {
		t.Error("expected Enabled=false when Debug=false and no env override")
	}
	if info.EnvOverride != nil {
		t.Errorf("expected EnvOverride=nil, got %+v", info.EnvOverride)
	}
}

func TestBuildDebugModeInfo_NilOptions(t *testing.T) {
	t.Setenv("LOG_LEVEL", "")
	os.Unsetenv("LOG_LEVEL")

	info := buildDebugModeInfo(nil)

	if !info.Enabled {
		t.Error("expected Enabled=true when ExecutionOptions is nil (default is debug)")
	}
	if info.EnvOverride != nil {
		t.Errorf("expected EnvOverride=nil, got %+v", info.EnvOverride)
	}
}

func TestBuildDebugModeInfo_EnvOverrideInfo(t *testing.T) {
	t.Setenv("LOG_LEVEL", "info")

	opts := &api.RenovateExecutionOptions{Debug: true}
	info := buildDebugModeInfo(opts)

	// Env override wins: LOG_LEVEL=info means debug is NOT enabled
	if info.Enabled {
		t.Error("expected Enabled=false when LOG_LEVEL=info overrides")
	}
	if info.EnvOverride == nil {
		t.Fatal("expected EnvOverride to be set")
	}
	if info.EnvOverride.Name != "LOG_LEVEL" {
		t.Errorf("expected EnvOverride.Name='LOG_LEVEL', got %q", info.EnvOverride.Name)
	}
	if info.EnvOverride.Value != "info" {
		t.Errorf("expected EnvOverride.Value='info', got %q", info.EnvOverride.Value)
	}
}

func TestBuildDebugModeInfo_EnvOverrideDebug(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")

	opts := &api.RenovateExecutionOptions{Debug: false}
	info := buildDebugModeInfo(opts)

	// Env override wins: LOG_LEVEL=debug means debug IS enabled
	if !info.Enabled {
		t.Error("expected Enabled=true when LOG_LEVEL=debug overrides")
	}
	if info.EnvOverride == nil {
		t.Fatal("expected EnvOverride to be set")
	}
	if info.EnvOverride.Name != "LOG_LEVEL" {
		t.Errorf("expected EnvOverride.Name='LOG_LEVEL', got %q", info.EnvOverride.Name)
	}
	if info.EnvOverride.Value != "debug" {
		t.Errorf("expected EnvOverride.Value='debug', got %q", info.EnvOverride.Value)
	}
}

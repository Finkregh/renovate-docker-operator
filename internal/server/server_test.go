package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/api"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/metrics"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/resilience"
	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/statestore"
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
	t.Setenv("RENOVATE_LOG_LEVEL", "")
	_ = os.Unsetenv("RENOVATE_LOG_LEVEL")

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
	t.Setenv("RENOVATE_LOG_LEVEL", "")
	_ = os.Unsetenv("RENOVATE_LOG_LEVEL")

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
	t.Setenv("RENOVATE_LOG_LEVEL", "")
	_ = os.Unsetenv("RENOVATE_LOG_LEVEL")

	info := buildDebugModeInfo(nil)

	if !info.Enabled {
		t.Error("expected Enabled=true when ExecutionOptions is nil (default is debug)")
	}
	if info.EnvOverride != nil {
		t.Errorf("expected EnvOverride=nil, got %+v", info.EnvOverride)
	}
}

func TestBuildDebugModeInfo_EnvOverrideInfo(t *testing.T) {
	t.Setenv("RENOVATE_LOG_LEVEL", "info")

	opts := &api.RenovateExecutionOptions{Debug: true}
	info := buildDebugModeInfo(opts)

	// Env override wins: RENOVATE_LOG_LEVEL=info means debug is NOT enabled
	if info.Enabled {
		t.Error("expected Enabled=false when RENOVATE_LOG_LEVEL=info overrides")
	}
	if info.EnvOverride == nil {
		t.Fatal("expected EnvOverride to be set")
	}
	if info.EnvOverride.Name != "RENOVATE_LOG_LEVEL" {
		t.Errorf("expected EnvOverride.Name='RENOVATE_LOG_LEVEL', got %q", info.EnvOverride.Name)
	}
	if info.EnvOverride.Value != "info" {
		t.Errorf("expected EnvOverride.Value='info', got %q", info.EnvOverride.Value)
	}
}

func TestBuildDebugModeInfo_EnvOverrideDebug(t *testing.T) {
	t.Setenv("RENOVATE_LOG_LEVEL", "debug")

	opts := &api.RenovateExecutionOptions{Debug: false}
	info := buildDebugModeInfo(opts)

	// Env override wins: RENOVATE_LOG_LEVEL=debug means debug IS enabled
	if !info.Enabled {
		t.Error("expected Enabled=true when RENOVATE_LOG_LEVEL=debug overrides")
	}
	if info.EnvOverride == nil {
		t.Fatal("expected EnvOverride to be set")
	}
	if info.EnvOverride.Name != "RENOVATE_LOG_LEVEL" {
		t.Errorf("expected EnvOverride.Name='RENOVATE_LOG_LEVEL', got %q", info.EnvOverride.Name)
	}
	if info.EnvOverride.Value != "debug" {
		t.Errorf("expected EnvOverride.Value='debug', got %q", info.EnvOverride.Value)
	}
}

// ---------------------------------------------------------------------------
// Breaker API tests
// ---------------------------------------------------------------------------

func TestGetBreakerState(t *testing.T) {
	mgr := resilience.New(resilience.Config{
		RapidFailThreshold: 5,
		RapidFailWindow:    5 * time.Minute,
	}, nil)

	s := &Server{resilience: mgr}
	router := mux.NewRouter()
	router.HandleFunc("/api/v1/breaker/state", s.getBreakerState).Methods("GET")

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/breaker/state", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var snap resilience.Snapshot
	if err := json.NewDecoder(w.Body).Decode(&snap); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if snap.State != resilience.StateClosed {
		t.Errorf("expected state=closed, got %q", snap.State)
	}
	if snap.WindowSeconds != 300 {
		t.Errorf("expected windowSeconds=300, got %d", snap.WindowSeconds)
	}
}

func TestResetBreaker(t *testing.T) {
	mgr := resilience.New(resilience.Config{
		RapidFailThreshold: 2,
	}, nil)
	// Trip the breaker by reporting rapid failures.
	mgr.Report("proj/a", resilience.SourceCron, resilience.OutcomeRapidFail, 0, 1)
	mgr.Report("proj/b", resilience.SourceCron, resilience.OutcomeRapidFail, 0, 1)
	// Enqueue a replay.
	_ = mgr.EnqueueWebhookReplay("proj/c")

	store := &fakeResetStore{scheduled: make(map[string]bool)}
	s := &Server{
		resilience: mgr,
		store:      store,
		logger:     slog.Default(),
	}
	router := mux.NewRouter()
	router.HandleFunc("/api/v1/breaker/reset", s.resetBreaker).Methods("POST")

	req := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/breaker/reset", nil)
	req.SetBasicAuth("admin", "pass")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		PreviousState    string   `json:"previousState"`
		ClearedBackoffs  int      `json:"clearedBackoffs"`
		ReplayedProjects []string `json:"replayedProjects"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if resp.PreviousState != "open" {
		t.Errorf("expected previousState=open, got %q", resp.PreviousState)
	}
	if resp.ClearedBackoffs != 2 {
		t.Errorf("expected clearedBackoffs=2, got %d", resp.ClearedBackoffs)
	}
	if !store.scheduled["proj/c"] {
		t.Error("expected proj/c to be marked as scheduled in state store")
	}
}

func TestBypassBreaker(t *testing.T) {
	mgr := resilience.New(resilience.Config{}, nil)
	s := &Server{
		resilience: mgr,
		logger:     slog.Default(),
	}
	router := mux.NewRouter()
	router.HandleFunc("/api/v1/breaker/bypass/{project:.+}", s.bypassBreaker).Methods("POST")

	req := httptest.NewRequestWithContext(context.Background(), "POST", "/api/v1/breaker/bypass/org/repo", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the manual flag is set.
	allowed, _, _ := mgr.AllowDispatch("org/repo", resilience.SourceCron)
	if !allowed {
		t.Error("expected dispatch allowed after bypass, got denied")
	}
}

func TestMetricsEndpoint(t *testing.T) {
	rec, err := metrics.New(metrics.Config{ProjectLabelMode: metrics.ModeAll})
	if err != nil {
		t.Fatalf("failed to create recorder: %v", err)
	}
	mgr := resilience.New(resilience.Config{}, nil)
	s := &Server{metrics: rec, resilience: mgr}
	router := mux.NewRouter()
	router.HandleFunc("/metrics", s.metricsHandler).Methods("GET")

	req := httptest.NewRequestWithContext(context.Background(), "GET", "/metrics", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "renovate_circuit_breaker_open") {
		t.Error("expected prometheus metric renovate_circuit_breaker_open in output")
	}
}

func TestActorFromRequest_BasicAuth(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	req.SetBasicAuth("ops-user", "secret")
	if got := actorFromRequest(req); got != "ops-user" {
		t.Errorf("expected actor=ops-user, got %q", got)
	}
}

func TestActorFromRequest_Anonymous(t *testing.T) {
	req := httptest.NewRequest("POST", "/", nil)
	if got := actorFromRequest(req); got != "anonymous" {
		t.Errorf("expected actor=anonymous, got %q", got)
	}
}

// --- Fake store for reset tests ---

type fakeResetStore struct {
	statestore.RenovateJobManager
	scheduled map[string]bool
}

func (f *fakeResetStore) ListRenovateJobsFull(_ context.Context) ([]api.RenovateJob, error) {
	return []api.RenovateJob{{Name: "renovate"}}, nil
}

func (f *fakeResetStore) UpdateProjectStatus(_ context.Context, project string, _ statestore.RenovateJobIdentifier, upd *statestore.RenovateStatusUpdate) error {
	if upd.Status == api.JobStatusScheduled {
		f.scheduled[project] = true
	}
	return nil
}

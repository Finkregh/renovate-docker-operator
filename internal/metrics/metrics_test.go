package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func TestNew_AllMode(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeAll})
	if err != nil {
		t.Fatalf("New(all) error: %v", err)
	}
	if r.Mode() != ModeAll {
		t.Fatalf("expected mode all, got %v", r.Mode())
	}
}

func TestNew_BreakerMode(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeBreaker})
	if err != nil {
		t.Fatalf("New(breaker) error: %v", err)
	}
	if r.Mode() != ModeBreaker {
		t.Fatalf("expected mode breaker, got %v", r.Mode())
	}
}

func TestNew_OffMode(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeOff})
	if err != nil {
		t.Fatalf("New(off) error: %v", err)
	}
	if r.Mode() != ModeOff {
		t.Fatalf("expected mode off, got %v", r.Mode())
	}
}

func TestNew_EmptyDefaultsToAll(t *testing.T) {
	r, err := New(Config{})
	if err != nil {
		t.Fatalf("New(empty) error: %v", err)
	}
	if r.Mode() != ModeAll {
		t.Fatalf("expected mode all, got %v", r.Mode())
	}
}

func TestNew_InvalidMode(t *testing.T) {
	_, err := New(Config{ProjectLabelMode: "bogus"})
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestParseProjectLabelMode(t *testing.T) {
	cases := []struct {
		input string
		want  ProjectLabelMode
		err   bool
	}{
		{"", ModeAll, false},
		{"all", ModeAll, false},
		{"breaker", ModeBreaker, false},
		{"off", ModeOff, false},
		{"invalid", "", true},
	}
	for _, tc := range cases {
		got, err := ParseProjectLabelMode(tc.input)
		if tc.err && err == nil {
			t.Errorf("ParseProjectLabelMode(%q): expected error", tc.input)
		}
		if !tc.err && err != nil {
			t.Errorf("ParseProjectLabelMode(%q): unexpected error: %v", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("ParseProjectLabelMode(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestAllMode_RecordDispatch_HasProjectLabel(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeAll})
	if err != nil {
		t.Fatal(err)
	}
	r.RecordDispatch("my/repo", "ok")

	families, err := r.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range families {
		if f.GetName() == "renovate_dispatch_total" {
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "project" && l.GetValue() == "my/repo" {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Error("expected project label 'my/repo' on renovate_dispatch_total in all mode")
	}
}

func TestBreakerMode_SuccessDispatch_NoProjectLabel(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeBreaker})
	if err != nil {
		t.Fatal(err)
	}
	r.RecordDispatch("my/repo", "ok")

	families, err := r.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "renovate_dispatch_total" {
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "project" {
						t.Errorf("dispatch_total in breaker mode should NOT have project label, found value=%q", l.GetValue())
					}
				}
			}
		}
	}
}

func TestBreakerMode_ContainerExit_HasProjectLabel(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeBreaker})
	if err != nil {
		t.Fatal(err)
	}
	r.RecordContainerExit("my/repo", "executor", "renovate", "rapid_failure")

	families, err := r.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range families {
		if f.GetName() == "renovate_container_exits_total" {
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "project" && l.GetValue() == "my/repo" {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Error("expected project label on container_exits_total in breaker mode")
	}
}

func TestOffMode_NoProjectLabelAnywhere(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeOff})
	if err != nil {
		t.Fatal(err)
	}
	// Record various metrics.
	r.RecordDispatch("my/repo", "ok")
	r.RecordContainerExit("my/repo", "executor", "renovate", "success")
	r.SetProjectBackoff("my/repo", 30.0)
	r.SetConsecutiveFailures("my/repo", 5)

	families, err := r.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "project" {
					t.Errorf("off mode: metric %s has project label with value %q", f.GetName(), l.GetValue())
				}
			}
		}
	}
}

func TestSetBreakerState(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeAll})
	if err != nil {
		t.Fatal(err)
	}
	r.SetBreakerState("open")
	val := testutil.ToFloat64(r.breakerOpen)
	if val != 1 {
		t.Errorf("expected breaker_open=1, got %v", val)
	}
	r.SetBreakerState("closed")
	val = testutil.ToFloat64(r.breakerOpen)
	if val != 0 {
		t.Errorf("expected breaker_open=0, got %v", val)
	}
}

func TestSetQueueDepth(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeAll})
	if err != nil {
		t.Fatal(err)
	}
	r.SetQueueDepth(42)
	val := testutil.ToFloat64(r.queueDepth)
	if val != 42 {
		t.Errorf("expected queue_depth=42, got %v", val)
	}
}

func TestObserveContainerDuration(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeAll})
	if err != nil {
		t.Fatal(err)
	}
	r.ObserveContainerDuration("my/repo", "success", 5*time.Second)

	families, err := r.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range families {
		if f.GetName() == "renovate_container_duration_seconds" {
			for _, m := range f.GetMetric() {
				if m.GetHistogram() != nil && m.GetHistogram().GetSampleCount() == 1 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("expected one histogram observation for container_duration_seconds")
	}
}

func TestHTTPMiddleware(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeAll})
	if err != nil {
		t.Fatal(err)
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	wrapped := r.WrapHandler("/api/v1/test", inner)

	req := httptest.NewRequest("GET", "/api/v1/test", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	families, err := r.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range families {
		if f.GetName() == "http_request_duration_seconds" {
			for _, m := range f.GetMetric() {
				if m.GetHistogram() != nil && m.GetHistogram().GetSampleCount() == 1 {
					found = true
					// Verify labels.
					labels := map[string]string{}
					for _, l := range m.GetLabel() {
						labels[l.GetName()] = l.GetValue()
					}
					if labels["route"] != "/api/v1/test" {
						t.Errorf("expected route=/api/v1/test, got %q", labels["route"])
					}
					if labels["method"] != "GET" {
						t.Errorf("expected method=GET, got %q", labels["method"])
					}
					if labels["code"] != "200" {
						t.Errorf("expected code=200, got %q", labels["code"])
					}
				}
			}
		}
	}
	if !found {
		t.Error("expected one histogram observation for http_request_duration_seconds")
	}
}

func TestHandler_Returns200(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeAll})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("expected text/plain content type, got %q", ct)
	}
}

func TestRecordBreakerTransition(t *testing.T) {
	r, err := New(Config{ProjectLabelMode: ModeAll})
	if err != nil {
		t.Fatal(err)
	}
	r.RecordBreakerTransition("open")
	r.RecordBreakerTransition("open")
	r.RecordBreakerTransition("closed")

	families, err := r.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "renovate_circuit_breaker_transitions_total" {
			for _, m := range f.GetMetric() {
				labels := labelMap(m.GetLabel())
				switch labels["to"] {
				case "open":
					if m.GetCounter().GetValue() != 2 {
						t.Errorf("expected 2 open transitions, got %v", m.GetCounter().GetValue())
					}
				case "closed":
					if m.GetCounter().GetValue() != 1 {
						t.Errorf("expected 1 closed transition, got %v", m.GetCounter().GetValue())
					}
				}
			}
		}
	}
}

func labelMap(labels []*dto.LabelPair) map[string]string {
	m := map[string]string{}
	for _, l := range labels {
		m[l.GetName()] = l.GetValue()
	}
	return m
}

package scheduler

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/internal/resilience"
)

// --- Fakes ---

type fakeGate struct {
	// decisions maps project name → (allowed, retryAfter, reason)
	decisions map[string]struct {
		allowed    bool
		retryAfter time.Duration
		reason     string
	}
}

func (g *fakeGate) AllowDispatch(project string, _ resilience.Source) (bool, time.Duration, string) {
	d, ok := g.decisions[project]
	if !ok {
		// Default: allow
		return true, 0, ""
	}
	return d.allowed, d.retryAfter, d.reason
}

type dispatchCall struct {
	project string
	result  string
}

type fakeMetrics struct {
	dispatches []dispatchCall
	queueDepth int
}

func (m *fakeMetrics) RecordDispatch(project, result string) {
	m.dispatches = append(m.dispatches, dispatchCall{project: project, result: result})
}

func (m *fakeMetrics) SetQueueDepth(n int) {
	m.queueDepth = n
}

// --- Tests ---

func TestDispatchProjects_GateAllows(t *testing.T) {
	gate := &fakeGate{
		decisions: map[string]struct {
			allowed    bool
			retryAfter time.Duration
			reason     string
		}{
			"org/repo-a": {allowed: true},
			"org/repo-b": {allowed: true},
		},
	}
	metrics := &fakeMetrics{}
	d := NewDispatcher(gate, metrics, slog.Default())

	var dispatched []string
	fn := func(project string) error {
		dispatched = append(dispatched, project)
		return nil
	}

	count, err := d.DispatchProjects([]string{"org/repo-a", "org/repo-b"}, resilience.SourceCron, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 dispatched, got %d", count)
	}
	if len(dispatched) != 2 {
		t.Fatalf("expected fn called 2 times, got %d", len(dispatched))
	}
	if len(metrics.dispatches) != 2 {
		t.Fatalf("expected 2 metric calls, got %d", len(metrics.dispatches))
	}
	for _, dc := range metrics.dispatches {
		if dc.result != "dispatched" {
			t.Fatalf("expected result 'dispatched', got %q", dc.result)
		}
	}
}

func TestDispatchProjects_GateDenies_BreakerOpen(t *testing.T) {
	gate := &fakeGate{
		decisions: map[string]struct {
			allowed    bool
			retryAfter time.Duration
			reason     string
		}{
			"org/repo-a": {allowed: false, retryAfter: 0, reason: "breaker_open"},
		},
	}
	metrics := &fakeMetrics{}
	d := NewDispatcher(gate, metrics, slog.Default())

	var dispatched []string
	fn := func(project string) error {
		dispatched = append(dispatched, project)
		return nil
	}

	count, err := d.DispatchProjects([]string{"org/repo-a"}, resilience.SourceCron, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 dispatched, got %d", count)
	}
	if len(dispatched) != 0 {
		t.Fatalf("expected fn NOT called, got %d calls", len(dispatched))
	}
	if len(metrics.dispatches) != 1 {
		t.Fatalf("expected 1 metric call, got %d", len(metrics.dispatches))
	}
	if metrics.dispatches[0].result != "skipped_breaker_open" {
		t.Fatalf("expected 'skipped_breaker_open', got %q", metrics.dispatches[0].result)
	}
	if metrics.dispatches[0].project != "org/repo-a" {
		t.Fatalf("expected project 'org/repo-a', got %q", metrics.dispatches[0].project)
	}
}

func TestDispatchProjects_GateDenies_Backoff(t *testing.T) {
	gate := &fakeGate{
		decisions: map[string]struct {
			allowed    bool
			retryAfter time.Duration
			reason     string
		}{
			"org/repo-x": {allowed: false, retryAfter: 30 * time.Second, reason: "project_backoff"},
		},
	}
	metrics := &fakeMetrics{}
	d := NewDispatcher(gate, metrics, slog.Default())

	var dispatched []string
	fn := func(project string) error {
		dispatched = append(dispatched, project)
		return nil
	}

	count, err := d.DispatchProjects([]string{"org/repo-x"}, resilience.SourceCron, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 dispatched, got %d", count)
	}
	if len(dispatched) != 0 {
		t.Fatalf("expected fn NOT called, got %d calls", len(dispatched))
	}
	if len(metrics.dispatches) != 1 {
		t.Fatalf("expected 1 metric call, got %d", len(metrics.dispatches))
	}
	if metrics.dispatches[0].result != "skipped_project_backoff" {
		t.Fatalf("expected 'skipped_project_backoff', got %q", metrics.dispatches[0].result)
	}
}

func TestDispatchProjects_NilGate_DispatchesAll(t *testing.T) {
	metrics := &fakeMetrics{}
	d := NewDispatcher(nil, metrics, slog.Default())

	var dispatched []string
	fn := func(project string) error {
		dispatched = append(dispatched, project)
		return nil
	}

	count, err := d.DispatchProjects([]string{"a", "b", "c"}, resilience.SourceCron, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 dispatched, got %d", count)
	}
	if len(dispatched) != 3 {
		t.Fatalf("expected fn called 3 times, got %d", len(dispatched))
	}
	// All should be 'dispatched'
	for _, dc := range metrics.dispatches {
		if dc.result != "dispatched" {
			t.Fatalf("expected result 'dispatched', got %q", dc.result)
		}
	}
}

func TestDispatchProjects_NilMetrics_NoPanic(t *testing.T) {
	gate := &fakeGate{
		decisions: map[string]struct {
			allowed    bool
			retryAfter time.Duration
			reason     string
		}{
			"org/x": {allowed: false, reason: "breaker_open"},
		},
	}
	d := NewDispatcher(gate, nil, slog.Default())

	fn := func(_ string) error { return nil }

	// Should not panic with nil metrics
	count, err := d.DispatchProjects([]string{"org/x", "org/y"}, resilience.SourceCron, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// org/x denied, org/y allowed (no decision = default allow)
	if count != 1 {
		t.Fatalf("expected 1 dispatched, got %d", count)
	}
}

func TestDispatchProjects_MixedDecisions(t *testing.T) {
	gate := &fakeGate{
		decisions: map[string]struct {
			allowed    bool
			retryAfter time.Duration
			reason     string
		}{
			"allowed-1":      {allowed: true},
			"denied-breaker": {allowed: false, reason: "breaker_open"},
			"denied-backoff": {allowed: false, retryAfter: time.Minute, reason: "project_backoff"},
			"allowed-2":      {allowed: true},
		},
	}
	metrics := &fakeMetrics{}
	d := NewDispatcher(gate, metrics, slog.Default())

	var dispatched []string
	fn := func(project string) error {
		dispatched = append(dispatched, project)
		return nil
	}

	projects := []string{"allowed-1", "denied-breaker", "denied-backoff", "allowed-2"}
	count, err := d.DispatchProjects(projects, resilience.SourceCron, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 dispatched, got %d", count)
	}
	if len(dispatched) != 2 {
		t.Fatalf("expected fn called 2 times, got %d", len(dispatched))
	}
	if dispatched[0] != "allowed-1" || dispatched[1] != "allowed-2" {
		t.Fatalf("unexpected dispatched order: %v", dispatched)
	}

	// Check metrics: 4 calls total
	if len(metrics.dispatches) != 4 {
		t.Fatalf("expected 4 metric calls, got %d", len(metrics.dispatches))
	}
	expected := []dispatchCall{
		{project: "allowed-1", result: "dispatched"},
		{project: "denied-breaker", result: "skipped_breaker_open"},
		{project: "denied-backoff", result: "skipped_project_backoff"},
		{project: "allowed-2", result: "dispatched"},
	}
	for i, exp := range expected {
		if metrics.dispatches[i] != exp {
			t.Fatalf("metric[%d]: expected %+v, got %+v", i, exp, metrics.dispatches[i])
		}
	}
}

func TestDispatchProjects_FnError_ContinuesAll(t *testing.T) {
	metrics := &fakeMetrics{}
	d := NewDispatcher(nil, metrics, slog.Default())

	errBoom := errors.New("boom")
	callCount := 0
	fn := func(project string) error {
		callCount++
		if project == "bad" {
			return errBoom
		}
		return nil
	}

	count, err := d.DispatchProjects([]string{"good", "bad", "good2"}, resilience.SourceCron, fn)
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
	// All three should still be attempted and counted as dispatched
	if count != 3 {
		t.Fatalf("expected 3 dispatched, got %d", count)
	}
	if callCount != 3 {
		t.Fatalf("expected fn called 3 times, got %d", callCount)
	}
}

func TestDispatchProjects_EmptyList(t *testing.T) {
	metrics := &fakeMetrics{}
	d := NewDispatcher(nil, metrics, slog.Default())

	count, err := d.DispatchProjects(nil, resilience.SourceCron, func(string) error { return nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0, got %d", count)
	}
	if len(metrics.dispatches) != 0 {
		t.Fatalf("expected 0 metric calls, got %d", len(metrics.dispatches))
	}
}

func TestDispatchProjects_WebhookSource(t *testing.T) {
	// Verify the source is passed through to the gate
	var receivedSource resilience.Source
	gate := &sourceCapturingGate{
		capturedSource: &receivedSource,
	}
	d := NewDispatcher(gate, nil, slog.Default())

	_, _ = d.DispatchProjects([]string{"proj"}, resilience.SourceWebhook, func(string) error { return nil })

	if receivedSource != resilience.SourceWebhook {
		t.Fatalf("expected source %q passed to gate, got %q", resilience.SourceWebhook, receivedSource)
	}
}

// sourceCapturingGate captures the source passed to AllowDispatch.
type sourceCapturingGate struct {
	capturedSource *resilience.Source
}

func (g *sourceCapturingGate) AllowDispatch(_ string, source resilience.Source) (bool, time.Duration, string) {
	*g.capturedSource = source
	return true, 0, ""
}

// Package resilience implements the circuit breaker, per-project exponential
// backoff, and manual-bypass semantics described in
// unipi/docs/specs/2026-07-29-resilience-metrics.md.
//
// Design principles:
//   - All state is in-memory and process-local. Container restart = clean slate.
//   - The Manager is safe for concurrent use. All exported methods take the
//     internal mutex; no method holds it across I/O.
//   - The wall clock is injected via [Config.Clock] to keep tests deterministic.
//   - No metrics are emitted from this package directly — callers read a
//     [Snapshot] and translate to Prometheus gauges/counters in the metrics
//     package.
package resilience

import (
	"errors"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"time"
)

// State is the current breaker state.
type State string

const (
	// StateClosed means dispatches are allowed (subject to per-project backoff).
	StateClosed State = "closed"
	// StateOpen means the global breaker has tripped: cron and webhook
	// dispatches are blocked. Manual dispatches still bypass via [Manager.MarkManual].
	StateOpen State = "open"
)

// Source identifies which caller is asking [Manager.AllowDispatch] whether a
// project may run. It controls whether the breaker gate applies.
type Source string

const (
	// SourceCron represents scheduled dispatch from the cron loop.
	// Blocked by both breaker and per-project backoff.
	SourceCron Source = "cron"
	// SourceWebhook represents dispatch triggered by an inbound platform webhook.
	// Blocked by both breaker and per-project backoff. Callers that see
	// [Manager.AllowDispatch] deny should enqueue via [Manager.EnqueueWebhookReplay]
	// rather than return 503 (spec §5.8).
	SourceWebhook Source = "webhook"
	// SourceManual represents a user-initiated dispatch. Ignores both the
	// breaker and per-project backoff — but only after [Manager.MarkManual]
	// has set the single-shot bypass flag for that project (spec §5.6).
	SourceManual Source = "manual"
)

// Outcome classifies a completed container run for [Manager.Report].
type Outcome string

const (
	// OutcomeSuccess: exit code 0.
	OutcomeSuccess Outcome = "success"
	// OutcomeRapidFail: non-zero exit AND container runtime < [Config.FailureMinRuntime].
	// Feeds both the per-project backoff and the global rapid-fail window.
	OutcomeRapidFail Outcome = "rapid_failure"
	// OutcomeSlowFail: non-zero exit AND runtime ≥ [Config.FailureMinRuntime].
	// Feeds per-project backoff only; does NOT count toward the breaker
	// (spec §5.2 — a slow crash is a real error but not a "run loop of doom").
	OutcomeSlowFail Outcome = "slow_failure"
)

// DiscoveryProject is the synthetic project name used when the discovery
// container fails. Discovery failures feed the breaker (spec §5.2) but are
// filtered out of per-project UI listings by convention.
const DiscoveryProject = "__discovery__"

// Config captures the tunable parameters for the resilience Manager.
// Zero values are replaced with defaults in [New].
type Config struct {
	// FailureMinRuntime is the container-runtime threshold that separates
	// rapid failures (< this) from slow failures (≥ this). Default: 30s.
	FailureMinRuntime time.Duration
	// BackoffBase is the initial per-project backoff after the first
	// consecutive failure. Default: 30s.
	BackoffBase time.Duration
	// BackoffMax caps the per-project backoff. Default: 30m.
	BackoffMax time.Duration
	// RapidFailWindow is the sliding window in which rapid failures are
	// counted toward the global breaker. Default: 5m.
	RapidFailWindow time.Duration
	// RapidFailThreshold is the number of rapid failures within
	// [RapidFailWindow] that trips the breaker. Default: 10.
	RapidFailThreshold int
	// ReplayQueueCap bounds the webhook replay queue (spec §5.8). Default: 10000.
	ReplayQueueCap int
	// Clock is the wall-clock source. Tests inject a fake clock; production
	// leaves this nil and [New] substitutes [time.Now].
	Clock func() time.Time
	// Rand is the random source for jitter. Tests can inject a deterministic
	// source. If nil, a default source is used.
	Rand *rand.Rand
}

func (c Config) withDefaults() Config {
	if c.FailureMinRuntime <= 0 {
		c.FailureMinRuntime = 30 * time.Second
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 30 * time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 30 * time.Minute
	}
	if c.RapidFailWindow <= 0 {
		c.RapidFailWindow = 5 * time.Minute
	}
	if c.RapidFailThreshold <= 0 {
		c.RapidFailThreshold = 10
	}
	if c.ReplayQueueCap <= 0 {
		c.ReplayQueueCap = 10000
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.Rand == nil {
		c.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return c
}

// projectState is the per-project bookkeeping held under the Manager mutex.
type projectState struct {
	consecutiveFailures int
	nextAllowedAt       time.Time
	lastExitCode        int
	lastReportAt        time.Time
	manualPending       bool
}

// Manager holds all resilience state. It is safe for concurrent use.
// Construct via [New]; do not use the zero value.
type Manager struct {
	cfg    Config
	logger *slog.Logger

	mu sync.Mutex
	// projects maps project name → per-project state. Includes the synthetic
	// [DiscoveryProject] entry when discovery has failed.
	projects map[string]*projectState
	// rapidFails is a chronologically ordered slice of rapid-failure timestamps
	// within the current window. Trimmed on every mutation.
	rapidFails []time.Time
	// state is the current breaker state.
	state State
	// openSince is set when the breaker trips; zero when closed.
	openSince time.Time
	// openReason describes why the breaker last tripped (for UI/logs).
	openReason string
	// pendingReplay is the set of projects whose webhook was accepted with 202
	// while the breaker was open or their per-project backoff was active.
	// Drained by [Manager.Reset] and opportunistically by successful
	// [Manager.AllowDispatch] returns.
	pendingReplay map[string]struct{}
	// lastReset / lastTripped are informational timestamps surfaced in [Snapshot].
	lastReset   time.Time
	lastTripped time.Time
}

// New constructs a Manager with defaults filled in for any zero-valued config field.
func New(cfg Config, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		cfg:           cfg.withDefaults(),
		logger:        logger,
		projects:      make(map[string]*projectState),
		state:         StateClosed,
		pendingReplay: make(map[string]struct{}),
	}
}

// Config returns the resolved configuration (with defaults filled in).
func (m *Manager) Config() Config {
	return m.cfg
}

// ProjectSnapshot is the per-project state included in [Snapshot].
type ProjectSnapshot struct {
	Project             string    `json:"project"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	NextAllowedAt       time.Time `json:"nextAllowedAt"`
	LastExitCode        int       `json:"lastExitCode"`
	LastReportAt        time.Time `json:"lastReportAt"`
	ManualPending       bool      `json:"manualPending"`
}

// Snapshot is a point-in-time view of the Manager's state, safe to serialize
// for the UI or translate into metrics. Callers may mutate the returned value
// freely — it is a deep copy.
type Snapshot struct {
	State              State                      `json:"state"`
	OpenSince          time.Time                  `json:"openSince"`
	OpenReason         string                     `json:"openReason"`
	RapidFailCount     int                        `json:"rapidFailures5m"`
	WindowSeconds      int                        `json:"windowSeconds"`
	PendingReplayCount int                        `json:"pendingReplayCount"`
	LastReset          time.Time                  `json:"lastReset"`
	LastTripped        time.Time                  `json:"lastTripped"`
	Projects           map[string]ProjectSnapshot `json:"projects"`
}

// ResetSummary is returned by [Manager.Reset] so the caller can log the outcome
// and re-schedule any projects that were queued via webhook replay while the
// breaker was open (spec §5.7).
type ResetSummary struct {
	// PreviousState is the breaker state prior to reset.
	PreviousState State
	// ReplayedProjects is the drained webhook replay queue. The caller is
	// expected to mark each of these projects as JobStatusScheduled.
	ReplayedProjects []string
	// ClearedBackoffs is the number of projects whose per-project backoff was
	// active (had non-zero consecutiveFailures) when reset ran.
	ClearedBackoffs int
}

// ErrReplayQueueFull is returned by [Manager.EnqueueWebhookReplay] when the
// bounded queue (see [Config.ReplayQueueCap]) is at capacity. Callers should
// treat this as a rare backstop and surface 503 to the webhook platform.
var ErrReplayQueueFull = errors.New("resilience: webhook replay queue full")

// ---------- T2: Per-project backoff + Report ----------

// Report records the outcome of a container run. It updates per-project
// backoff state. Rapid failures also feed the global breaker window (T3).
func (m *Manager) Report(project string, _ Source, outcome Outcome, _ time.Duration, exitCode int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.cfg.Clock()

	ps := m.getOrCreateProject(project)
	ps.lastExitCode = exitCode
	ps.lastReportAt = now

	switch outcome {
	case OutcomeSuccess:
		ps.consecutiveFailures = 0
		ps.nextAllowedAt = time.Time{}
	case OutcomeRapidFail:
		ps.consecutiveFailures++
		ps.nextAllowedAt = now.Add(m.computeBackoff(ps.consecutiveFailures))
	case OutcomeSlowFail:
		ps.consecutiveFailures++
		ps.nextAllowedAt = now.Add(m.computeBackoff(ps.consecutiveFailures))
	}
}

// computeBackoff calculates the backoff duration for a given failure count.
// Formula: base * 2^(n-1), capped at max, with ±20% uniform jitter.
// Must be called with m.mu held (accesses m.cfg.Rand).
func (m *Manager) computeBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	exp := math.Pow(2, float64(failures-1))
	raw := float64(m.cfg.BackoffBase) * exp
	if raw > float64(m.cfg.BackoffMax) {
		raw = float64(m.cfg.BackoffMax)
	}
	// ±20% jitter: multiply by [0.8, 1.2)
	jitter := 0.8 + m.cfg.Rand.Float64()*0.4
	return time.Duration(raw * jitter)
}

// getOrCreateProject returns the per-project state, creating it if needed.
// Must be called with m.mu held.
func (m *Manager) getOrCreateProject(project string) *projectState {
	ps, ok := m.projects[project]
	if !ok {
		ps = &projectState{}
		m.projects[project] = ps
	}
	return ps
}

// AllowDispatch reports whether the given project may be dispatched right now.
// In this stage (T2), only per-project backoff is checked. Breaker and manual
// bypass are added in T3/T4.
func (m *Manager) AllowDispatch(project string, _ Source) (allowed bool, retryAfter time.Duration, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.cfg.Clock()
	ps := m.getOrCreateProject(project)

	// Per-project backoff gate.
	if !ps.nextAllowedAt.IsZero() && now.Before(ps.nextAllowedAt) {
		remaining := ps.nextAllowedAt.Sub(now)
		return false, remaining, "project_backoff"
	}

	return true, 0, ""
}

// MarkManual arms a single-shot bypass for the project. Stub — implemented in T4.
func (m *Manager) MarkManual(_ string) {}

// EnqueueWebhookReplay adds the project to the replay queue. Stub — implemented in T4.
func (m *Manager) EnqueueWebhookReplay(_ string) error { return nil }

// Reset performs a full resilience reset. Stub — implemented in T4.
func (m *Manager) Reset(_ string) ResetSummary {
	return ResetSummary{PreviousState: m.state}
}

// Snapshot returns a deep copy of the current state. Partial — fully implemented in T4.
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	snap := Snapshot{
		State:         m.state,
		WindowSeconds: int(m.cfg.RapidFailWindow / time.Second),
		Projects:      make(map[string]ProjectSnapshot, len(m.projects)),
	}

	for name, ps := range m.projects {
		snap.Projects[name] = ProjectSnapshot{
			Project:             name,
			ConsecutiveFailures: ps.consecutiveFailures,
			NextAllowedAt:       ps.nextAllowedAt,
			LastExitCode:        ps.lastExitCode,
			LastReportAt:        ps.lastReportAt,
			ManualPending:       ps.manualPending,
		}
	}

	return snap
}

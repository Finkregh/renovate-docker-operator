package resilience

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// fakeClock provides a deterministic clock for tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// testManager creates a Manager with a fake clock and deterministic random
// source. Returns both so the caller can advance time.
func testManager(t *testing.T, opts ...func(*Config)) (*Manager, *fakeClock) {
	t.Helper()
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg := Config{
		Clock: clock.Now,
		Rand:  rand.New(rand.NewSource(42)),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return New(cfg, nil), clock
}

// suppress unused import
var _ = fmt.Sprintf

// ---------- T1: Constructor defaults ----------

func TestNew_Defaults(t *testing.T) {
	m, _ := testManager(t)
	c := m.Config()
	if c.FailureMinRuntime != 30*time.Second {
		t.Errorf("FailureMinRuntime = %v, want 30s", c.FailureMinRuntime)
	}
	if c.BackoffBase != 30*time.Second {
		t.Errorf("BackoffBase = %v, want 30s", c.BackoffBase)
	}
	if c.BackoffMax != 30*time.Minute {
		t.Errorf("BackoffMax = %v, want 30m", c.BackoffMax)
	}
	if c.RapidFailWindow != 5*time.Minute {
		t.Errorf("RapidFailWindow = %v, want 5m", c.RapidFailWindow)
	}
	if c.RapidFailThreshold != 10 {
		t.Errorf("RapidFailThreshold = %d, want 10", c.RapidFailThreshold)
	}
	if c.ReplayQueueCap != 10000 {
		t.Errorf("ReplayQueueCap = %d, want 10000", c.ReplayQueueCap)
	}
}

// ---------- T2: Per-project backoff ----------

func TestReport_RapidFail_IncrementsBackoff(t *testing.T) {
	m, clock := testManager(t)

	m.Report("proj/a", SourceCron, OutcomeRapidFail, 5*time.Second, 1)

	snap := m.Snapshot()
	ps, ok := snap.Projects["proj/a"]
	if !ok {
		t.Fatal("project not in snapshot after report")
	}
	if ps.ConsecutiveFailures != 1 {
		t.Errorf("failures = %d, want 1", ps.ConsecutiveFailures)
	}
	if ps.NextAllowedAt.Before(clock.Now()) || ps.NextAllowedAt.IsZero() {
		t.Errorf("nextAllowedAt should be in the future, got %v (now=%v)", ps.NextAllowedAt, clock.Now())
	}
}

func TestReport_SlowFail_IncrementsBackoff(t *testing.T) {
	m, clock := testManager(t)

	m.Report("proj/a", SourceCron, OutcomeSlowFail, 60*time.Second, 1)

	snap := m.Snapshot()
	ps := snap.Projects["proj/a"]
	if ps.ConsecutiveFailures != 1 {
		t.Errorf("failures = %d, want 1", ps.ConsecutiveFailures)
	}
	if ps.NextAllowedAt.Before(clock.Now()) || ps.NextAllowedAt.IsZero() {
		t.Errorf("nextAllowedAt should be in the future")
	}
}

func TestReport_Success_ResetsBackoff(t *testing.T) {
	m, _ := testManager(t)

	// Two failures then a success.
	m.Report("proj/a", SourceCron, OutcomeRapidFail, 5*time.Second, 1)
	m.Report("proj/a", SourceCron, OutcomeRapidFail, 5*time.Second, 1)
	m.Report("proj/a", SourceCron, OutcomeSuccess, 60*time.Second, 0)

	snap := m.Snapshot()
	ps := snap.Projects["proj/a"]
	if ps.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d, want 0 after success", ps.ConsecutiveFailures)
	}
	if !ps.NextAllowedAt.IsZero() {
		t.Errorf("nextAllowedAt should be zero after success, got %v", ps.NextAllowedAt)
	}
}

func TestBackoff_CapsAtMax(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.BackoffBase = 1 * time.Second
		c.BackoffMax = 10 * time.Second
	})

	// 20 consecutive failures should produce backoff capped at max.
	for i := 0; i < 20; i++ {
		m.Report("proj/cap", SourceCron, OutcomeRapidFail, 1*time.Second, 1)
		clock.Advance(1 * time.Millisecond) // tiny advance so timestamps differ
	}

	snap := m.Snapshot()
	ps := snap.Projects["proj/cap"]
	// nextAllowedAt relative to now should be at most max * 1.2 (upper jitter)
	remaining := ps.NextAllowedAt.Sub(clock.Now())
	maxWithJitter := time.Duration(float64(10*time.Second) * 1.2)
	if remaining > maxWithJitter {
		t.Errorf("backoff remaining = %v, exceeds max*1.2 = %v", remaining, maxWithJitter)
	}
}

func TestBackoff_JitterInBounds(t *testing.T) {
	// Run 100 samples and ensure all are within ±20% of the base backoff
	// (first failure: base * 2^0 = base).
	base := 10 * time.Second
	for i := 0; i < 100; i++ {
		m, clock := testManager(t, func(c *Config) {
			c.BackoffBase = base
			c.BackoffMax = 10 * time.Minute
			c.Rand = rand.New(rand.NewSource(int64(i)))
		})
		m.Report("proj/j", SourceCron, OutcomeRapidFail, 1*time.Second, 1)
		snap := m.Snapshot()
		ps := snap.Projects["proj/j"]
		got := ps.NextAllowedAt.Sub(clock.Now())
		lower := time.Duration(float64(base) * 0.8)
		upper := time.Duration(float64(base) * 1.2)
		if got < lower || got > upper {
			t.Errorf("sample %d: backoff %v not in [%v, %v]", i, got, lower, upper)
		}
	}
}

func TestBackoff_ExponentialGrowth(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.BackoffBase = 10 * time.Second
		c.BackoffMax = 10 * time.Minute
		c.Rand = rand.New(rand.NewSource(0))
	})

	var backoffs []time.Duration
	for i := 0; i < 5; i++ {
		before := clock.Now()
		m.Report("proj/exp", SourceCron, OutcomeRapidFail, 1*time.Second, 1)
		snap := m.Snapshot()
		ps := snap.Projects["proj/exp"]
		backoffs = append(backoffs, ps.NextAllowedAt.Sub(before))
		clock.Advance(ps.NextAllowedAt.Sub(before) + time.Second)
	}

	// Each backoff should be roughly double the previous (within jitter tolerance)
	for i := 1; i < len(backoffs); i++ {
		ratio := float64(backoffs[i]) / float64(backoffs[i-1])
		// With ±20% jitter on both, ratio could range from 2*0.8/1.2 ≈ 1.33 to 2*1.2/0.8 = 3.0
		if ratio < 1.3 || ratio > 3.1 {
			t.Errorf("backoff[%d]/backoff[%d] = %.2f, expected roughly 2x (with jitter: 1.3-3.0)", i, i-1, ratio)
		}
	}
}

func TestAllowDispatch_BackoffBlocks(t *testing.T) {
	m, _ := testManager(t)

	m.Report("proj/a", SourceCron, OutcomeRapidFail, 1*time.Second, 1)

	allowed, retryAfter, reason := m.AllowDispatch("proj/a", SourceCron)
	if allowed {
		t.Error("expected blocked by backoff")
	}
	if reason != "project_backoff" {
		t.Errorf("reason = %q, want project_backoff", reason)
	}
	if retryAfter <= 0 {
		t.Errorf("retryAfter should be positive, got %v", retryAfter)
	}
}

func TestAllowDispatch_BackoffExpires(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.BackoffBase = 5 * time.Second
		c.BackoffMax = 5 * time.Second
	})

	m.Report("proj/a", SourceCron, OutcomeRapidFail, 1*time.Second, 1)

	// Advance past max backoff + generous jitter allowance.
	clock.Advance(7 * time.Second)

	allowed, _, _ := m.AllowDispatch("proj/a", SourceCron)
	if !allowed {
		t.Error("expected allowed after backoff expires")
	}
}

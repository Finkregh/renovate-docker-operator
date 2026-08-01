package resilience

import (
	"errors"
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

// ---------- T3: Global rapid-fail breaker ----------

func TestBreaker_TripsOnThreshold(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.RapidFailThreshold = 3
		c.RapidFailWindow = 5 * time.Minute
	})

	for i := 0; i < 3; i++ {
		m.Report("proj/a", SourceCron, OutcomeRapidFail, 1*time.Second, 1)
		clock.Advance(10 * time.Second)
	}

	snap := m.Snapshot()
	if snap.State != StateOpen {
		t.Errorf("state = %q, want open after 3 rapid fails", snap.State)
	}
	if snap.OpenReason == "" {
		t.Error("openReason should be non-empty")
	}
}

func TestBreaker_DoesNotTrip_BelowThreshold(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.RapidFailThreshold = 5
		c.RapidFailWindow = 5 * time.Minute
	})

	for i := 0; i < 4; i++ {
		m.Report("proj/a", SourceCron, OutcomeRapidFail, 1*time.Second, 1)
		clock.Advance(10 * time.Second)
	}

	snap := m.Snapshot()
	if snap.State != StateClosed {
		t.Errorf("state = %q, want closed with only 4/5 rapid fails", snap.State)
	}
}

func TestBreaker_FailsOutsideWindowDoNotTrip(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.RapidFailThreshold = 3
		c.RapidFailWindow = 1 * time.Minute
	})

	// Spread 3 rapid fails over 3 minutes — only 1 per window.
	for i := 0; i < 3; i++ {
		m.Report("proj/a", SourceCron, OutcomeRapidFail, 1*time.Second, 1)
		clock.Advance(61 * time.Second) // each one falls outside window of previous
	}

	snap := m.Snapshot()
	if snap.State != StateClosed {
		t.Errorf("state = %q, want closed (fails spread outside window)", snap.State)
	}
}

func TestBreaker_SlowFailsDoNotTrip(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.RapidFailThreshold = 3
		c.RapidFailWindow = 5 * time.Minute
	})

	for i := 0; i < 10; i++ {
		m.Report("proj/a", SourceCron, OutcomeSlowFail, 60*time.Second, 1)
		clock.Advance(10 * time.Second)
	}

	snap := m.Snapshot()
	if snap.State != StateClosed {
		t.Errorf("state = %q, want closed (slow fails don't feed breaker)", snap.State)
	}
}

func TestBreaker_DiscoveryRapidFailsTrip(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.RapidFailThreshold = 3
		c.RapidFailWindow = 5 * time.Minute
	})

	for i := 0; i < 3; i++ {
		m.Report(DiscoveryProject, SourceCron, OutcomeRapidFail, 1*time.Second, 1)
		clock.Advance(10 * time.Second)
	}

	snap := m.Snapshot()
	if snap.State != StateOpen {
		t.Errorf("state = %q, want open (discovery rapid fails feed breaker)", snap.State)
	}
}

func TestBreaker_BlocksCronAndWebhook(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.RapidFailThreshold = 1
	})

	m.Report("proj/a", SourceCron, OutcomeRapidFail, 1*time.Second, 1)
	clock.Advance(1 * time.Second)

	// Cron blocked.
	allowed, _, reason := m.AllowDispatch("proj/b", SourceCron)
	if allowed {
		t.Error("cron should be blocked when breaker open")
	}
	if reason != "breaker_open" {
		t.Errorf("reason = %q, want breaker_open", reason)
	}

	// Webhook blocked.
	allowed, _, reason = m.AllowDispatch("proj/b", SourceWebhook)
	if allowed {
		t.Error("webhook should be blocked when breaker open")
	}
	if reason != "breaker_open" {
		t.Errorf("reason = %q, want breaker_open", reason)
	}
}

func TestBreaker_ResetCloses(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.RapidFailThreshold = 1
	})

	m.Report("proj/a", SourceCron, OutcomeRapidFail, 1*time.Second, 1)
	clock.Advance(1 * time.Second)

	summary := m.Reset("test")
	if summary.PreviousState != StateOpen {
		t.Errorf("PreviousState = %q, want open", summary.PreviousState)
	}

	snap := m.Snapshot()
	if snap.State != StateClosed {
		t.Errorf("state after reset = %q, want closed", snap.State)
	}
	if snap.RapidFailCount != 0 {
		t.Errorf("rapidFailCount after reset = %d, want 0", snap.RapidFailCount)
	}
}

// ---------- T4: Manual bypass, replay queue, snapshot ----------

func TestManual_ConsumedOnFirstCheck(t *testing.T) {
	m, _ := testManager(t)

	m.MarkManual("proj/a")

	// First check consumes the flag.
	allowed, _, _ := m.AllowDispatch("proj/a", SourceCron)
	if !allowed {
		t.Error("first AllowDispatch should be allowed (manual bypass)")
	}

	// Second check: flag consumed, back to normal behavior.
	snap := m.Snapshot()
	if snap.Projects["proj/a"].ManualPending {
		t.Error("manualPending should be false after consumption")
	}
}

func TestManual_BypassesOpenBreaker(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.RapidFailThreshold = 1
	})

	// Trip the breaker.
	m.Report("proj/a", SourceCron, OutcomeRapidFail, 1*time.Second, 1)
	clock.Advance(1 * time.Second)

	// Without manual flag: blocked.
	allowed, _, _ := m.AllowDispatch("proj/b", SourceCron)
	if allowed {
		t.Error("should be blocked without manual flag")
	}

	// With manual flag: allowed.
	m.MarkManual("proj/b")
	allowed, _, _ = m.AllowDispatch("proj/b", SourceCron)
	if !allowed {
		t.Error("should be allowed with manual flag, even when breaker open")
	}
}

func TestManual_BypassesBackoff(t *testing.T) {
	m, _ := testManager(t)

	// Create a backoff.
	m.Report("proj/a", SourceCron, OutcomeRapidFail, 1*time.Second, 1)

	// Without manual: blocked.
	allowed, _, reason := m.AllowDispatch("proj/a", SourceCron)
	if allowed {
		t.Error("should be blocked by backoff")
	}
	if reason != "project_backoff" {
		t.Errorf("reason = %q, want project_backoff", reason)
	}

	// With manual: allowed.
	m.MarkManual("proj/a")
	allowed, _, _ = m.AllowDispatch("proj/a", SourceCron)
	if !allowed {
		t.Error("manual should bypass backoff")
	}
}

func TestReplayQueue_Idempotent(t *testing.T) {
	m, _ := testManager(t)

	if err := m.EnqueueWebhookReplay("proj/a"); err != nil {
		t.Fatal(err)
	}
	if err := m.EnqueueWebhookReplay("proj/a"); err != nil {
		t.Fatal(err)
	}

	snap := m.Snapshot()
	if snap.PendingReplayCount != 1 {
		t.Errorf("pendingReplayCount = %d, want 1 (idempotent)", snap.PendingReplayCount)
	}
}

func TestReplayQueue_CapEnforced(t *testing.T) {
	m, _ := testManager(t, func(c *Config) {
		c.ReplayQueueCap = 3
	})

	for i := 0; i < 3; i++ {
		if err := m.EnqueueWebhookReplay(fmt.Sprintf("proj/%d", i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// 4th should fail.
	err := m.EnqueueWebhookReplay("proj/overflow")
	if !errors.Is(err, ErrReplayQueueFull) {
		t.Errorf("err = %v, want ErrReplayQueueFull", err)
	}
}

func TestReplayQueue_DrainedByReset(t *testing.T) {
	m, _ := testManager(t)

	_ = m.EnqueueWebhookReplay("proj/a")
	_ = m.EnqueueWebhookReplay("proj/b")

	summary := m.Reset("test")
	if len(summary.ReplayedProjects) != 2 {
		t.Errorf("replayed = %d, want 2", len(summary.ReplayedProjects))
	}

	snap := m.Snapshot()
	if snap.PendingReplayCount != 0 {
		t.Errorf("pendingReplayCount after reset = %d, want 0", snap.PendingReplayCount)
	}
}

func TestReplayQueue_OpportunisticDrain(t *testing.T) {
	m, _ := testManager(t)

	_ = m.EnqueueWebhookReplay("proj/a")

	// Project has no backoff, breaker closed → AllowDispatch should allow
	// and remove from replay queue.
	allowed, _, _ := m.AllowDispatch("proj/a", SourceCron)
	if !allowed {
		t.Fatal("should be allowed")
	}

	snap := m.Snapshot()
	if snap.PendingReplayCount != 0 {
		t.Errorf("pendingReplayCount = %d, want 0 (opportunistic drain)", snap.PendingReplayCount)
	}
}

func TestReset_ClearsEverything(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.RapidFailThreshold = 1
	})

	// Trip breaker.
	m.Report("proj/a", SourceCron, OutcomeRapidFail, 1*time.Second, 1)
	clock.Advance(1 * time.Second)

	// Set manual flag.
	m.MarkManual("proj/b")

	// Enqueue replay.
	_ = m.EnqueueWebhookReplay("proj/c")

	summary := m.Reset("operator")
	if summary.PreviousState != StateOpen {
		t.Errorf("PreviousState = %q, want open", summary.PreviousState)
	}
	if summary.ClearedBackoffs != 1 {
		t.Errorf("ClearedBackoffs = %d, want 1", summary.ClearedBackoffs)
	}
	if len(summary.ReplayedProjects) != 1 || summary.ReplayedProjects[0] != "proj/c" {
		t.Errorf("ReplayedProjects = %v, want [proj/c]", summary.ReplayedProjects)
	}

	// Everything should be clean.
	snap := m.Snapshot()
	if snap.State != StateClosed {
		t.Error("state should be closed after reset")
	}
	if snap.RapidFailCount != 0 {
		t.Error("rapid fail count should be 0")
	}
	if snap.PendingReplayCount != 0 {
		t.Error("pending replay count should be 0")
	}
	if len(snap.Projects) != 0 {
		t.Errorf("projects should be empty after reset, got %d", len(snap.Projects))
	}
}

func TestSnapshot_DeepCopy(t *testing.T) {
	m, _ := testManager(t)

	m.Report("proj/a", SourceCron, OutcomeRapidFail, 1*time.Second, 1)

	snap := m.Snapshot()

	// Mutate the snapshot.
	snap.Projects["proj/a"] = ProjectSnapshot{ConsecutiveFailures: 999}
	snap.Projects["fake"] = ProjectSnapshot{ConsecutiveFailures: 1}

	// Original should be unchanged.
	snap2 := m.Snapshot()
	if snap2.Projects["proj/a"].ConsecutiveFailures != 1 {
		t.Errorf("internal state was mutated via snapshot: got %d", snap2.Projects["proj/a"].ConsecutiveFailures)
	}
	if _, exists := snap2.Projects["fake"]; exists {
		t.Error("fake project should not exist in internal state")
	}
}

// ---------- Spec §5.9: No startup grace period ----------

func TestNoStartupGracePeriod(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.RapidFailThreshold = 3
		c.RapidFailWindow = 5 * time.Minute
	})

	// Fresh manager at t=0: immediate rapid fails should trip breaker.
	for i := 0; i < 3; i++ {
		m.Report("proj/boot", SourceCron, OutcomeRapidFail, 1*time.Second, 1)
		clock.Advance(1 * time.Second) // all within first 3 seconds
	}

	snap := m.Snapshot()
	if snap.State != StateOpen {
		t.Errorf("state = %q, want open — no startup grace period", snap.State)
	}
}

// ---------- Race safety ----------

func TestConcurrency_Race(t *testing.T) {
	m, clock := testManager(t, func(c *Config) {
		c.RapidFailThreshold = 100 // high threshold so we don't trip during the test
	})

	var wg sync.WaitGroup
	const goroutines = 20
	const iterations = 100

	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			project := fmt.Sprintf("proj/%d", id)
			for i := 0; i < iterations; i++ {
				switch i % 5 {
				case 0:
					m.Report(project, SourceCron, OutcomeRapidFail, 1*time.Second, 1)
				case 1:
					m.Report(project, SourceCron, OutcomeSuccess, 60*time.Second, 0)
				case 2:
					m.AllowDispatch(project, SourceCron)
				case 3:
					m.MarkManual(project)
					m.AllowDispatch(project, SourceManual)
				case 4:
					m.Snapshot()
				}
			}
		}(g)
	}

	// A separate goroutine advances the clock and does resets.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			clock.Advance(1 * time.Second)
			m.Reset("racer")
			_ = m.EnqueueWebhookReplay("proj/race")
		}
	}()

	wg.Wait()
}

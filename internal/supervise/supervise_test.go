package supervise

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Test helpers ---

type mockMetrics struct {
	mu     sync.Mutex
	ticks  map[string]int
	errors map[string]map[string]int
}

func newMockMetrics() *mockMetrics {
	return &mockMetrics{
		ticks:  make(map[string]int),
		errors: make(map[string]map[string]int),
	}
}

func (m *mockMetrics) IncTick(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ticks[name]++
}

func (m *mockMetrics) IncError(name, kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errors[name] == nil {
		m.errors[name] = make(map[string]int)
	}
	m.errors[name][kind]++
}

func (m *mockMetrics) tickCount(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ticks[name]
}

func (m *mockMetrics) errorCount(name, kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errors[name] == nil {
		return 0
	}
	return m.errors[name][kind]
}

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// --- Loop tests ---

func TestLoop_ImmediateFirstTick(t *testing.T) {
	t.Parallel()
	var called atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())

	body := func(ctx context.Context) error {
		called.Add(1)
		cancel() // cancel after first tick
		return ctx.Err()
	}

	var buf bytes.Buffer
	Loop(ctx, testLogger(&buf), "test-immediate", 1*time.Second, body,
		WithExitFn(func(int) {}))

	if got := called.Load(); got != 1 {
		t.Fatalf("expected body called once on immediate tick, got %d", got)
	}
}

func TestLoop_CtxCancelExitsCleanly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var exitCalled atomic.Int32

	body := func(ctx context.Context) error {
		cancel()
		return ctx.Err()
	}

	var buf bytes.Buffer
	Loop(ctx, testLogger(&buf), "test-cancel", 100*time.Millisecond, body,
		WithExitFn(func(_ int) { exitCalled.Add(1) }))

	if exitCalled.Load() != 0 {
		t.Fatal("exitFn should not have been called on clean ctx cancel")
	}
}

func TestLoop_BodyErrorLogsAndContinues(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	var exitCalled atomic.Int32
	metrics := newMockMetrics()

	body := func(bctx context.Context) error {
		n := calls.Add(1)
		if n >= 3 {
			cancel()
			return bctx.Err()
		}
		return errors.New("transient failure")
	}

	var buf bytes.Buffer
	Loop(ctx, testLogger(&buf), "test-error", 10*time.Millisecond, body,
		WithExitFn(func(_ int) { exitCalled.Add(1) }),
		WithMetrics(metrics))

	if exitCalled.Load() != 0 {
		t.Fatal("exitFn should not have been called on body errors")
	}
	if got := calls.Load(); got < 3 {
		t.Fatalf("expected at least 3 body calls, got %d", got)
	}
	// First 2 calls return errors, 3rd cancels context.
	if metrics.errorCount("test-error", "other") < 2 {
		t.Fatalf("expected at least 2 error recordings, got %d", metrics.errorCount("test-error", "other"))
	}
	if !strings.Contains(buf.String(), "supervise tick error") {
		t.Fatal("expected warn log for tick error")
	}
}

func TestLoop_BodyPanicIsFatal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var exitCode atomic.Int32
	exitCode.Store(-1)

	body := func(_ context.Context) error {
		panic("test panic value")
	}

	var buf bytes.Buffer
	Loop(ctx, testLogger(&buf), "test-panic", 1*time.Second, body,
		WithExitFn(func(code int) {
			exitCode.Store(int32(code))
			// Don't actually exit — allow loop to return for test purposes.
			// The defer/recover in executeTick catches and calls exitFn,
			// then returns, so Loop proceeds to the next tick. We use a
			// runtime.Goexit workaround below instead.
		}))

	// Loop will call exitFn and then the function returns from the deferred recovery.
	// The next tick will panic again, etc. But since exitFn doesn't actually exit,
	// we need to verify it was called at least once.
	if got := exitCode.Load(); got != 1 {
		t.Fatalf("expected exitFn called with code 1, got %d", got)
	}
	if !strings.Contains(buf.String(), "supervise loop panic") {
		t.Fatal("expected error log for panic")
	}
	if !strings.Contains(buf.String(), "test panic value") {
		t.Fatal("expected panic value in log")
	}
}

func TestLoop_BodyReturnsNilWithLiveCtxIsFatal(t *testing.T) {
	t.Parallel()
	var exitCode atomic.Int32
	exitCode.Store(-1)

	// Body returns nil immediately without ctx being cancelled.
	body := func(_ context.Context) error {
		return nil
	}

	var buf bytes.Buffer
	Loop(context.Background(), testLogger(&buf), "test-nil-return", 1*time.Second, body,
		WithExitFn(func(code int) { exitCode.Store(int32(code)) }))

	if got := exitCode.Load(); got != 1 {
		t.Fatalf("expected exitFn called with code 1, got %d", got)
	}
	if !strings.Contains(buf.String(), "supervise loop body returned unexpectedly") {
		t.Fatal("expected error log for unexpected nil return")
	}
}

func TestLoop_PerTickBodyNilReturnIsNotFatal(t *testing.T) {
	t.Parallel()
	var exitCode atomic.Int32
	exitCode.Store(-1)
	var calls atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())

	// A legitimate per-tick body: returns nil after each successful tick.
	// The ticker provides the looping; nil must NOT be treated as fatal.
	body := func(_ context.Context) error {
		if calls.Add(1) >= 2 {
			cancel() // let the loop exit cleanly after a couple of ticks
		}
		return nil
	}

	var buf bytes.Buffer
	Loop(ctx, testLogger(&buf), "test-pertick", 1*time.Millisecond, body,
		WithExitFn(func(code int) { exitCode.Store(int32(code)) }),
		WithPerTickBody())

	if got := exitCode.Load(); got != -1 {
		t.Fatalf("exitFn should not be called for a per-tick body returning nil, got code %d", got)
	}
	if strings.Contains(buf.String(), "supervise loop body returned unexpectedly") {
		t.Fatal("per-tick body returning nil must not log the unexpected-return fatal")
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("expected body to tick at least twice, got %d", got)
	}
}

func TestLoop_IntervalZeroCallsBodyOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var exitCalled atomic.Int32

	body := func(_ context.Context) error {
		calls.Add(1)
		return nil
	}

	var buf bytes.Buffer
	Loop(context.Background(), testLogger(&buf), "test-once", 0, body,
		WithExitFn(func(_ int) { exitCalled.Add(1) }))

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected body called exactly once with interval=0, got %d", got)
	}
	if exitCalled.Load() != 0 {
		t.Fatal("exitFn should not be called for interval=0 returning nil")
	}
}

func TestLoop_IntervalZeroWithError(t *testing.T) {
	t.Parallel()
	var exitCalled atomic.Int32
	metrics := newMockMetrics()

	body := func(_ context.Context) error {
		return errors.New("some error")
	}

	var buf bytes.Buffer
	Loop(context.Background(), testLogger(&buf), "test-once-err", 0, body,
		WithExitFn(func(_ int) { exitCalled.Add(1) }),
		WithMetrics(metrics))

	if exitCalled.Load() != 0 {
		t.Fatal("exitFn should not be called for interval=0 body error")
	}
	if metrics.tickCount("test-once-err") != 1 {
		t.Fatalf("expected 1 tick, got %d", metrics.tickCount("test-once-err"))
	}
	if metrics.errorCount("test-once-err", "other") != 1 {
		t.Fatalf("expected 1 error, got %d", metrics.errorCount("test-once-err", "other"))
	}
}

func TestLoop_Metrics(t *testing.T) {
	t.Parallel()
	metrics := newMockMetrics()
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32

	body := func(bctx context.Context) error {
		n := calls.Add(1)
		switch n {
		case 1:
			// Return a network error (classified as "network").
			return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		case 2:
			// Return a generic error (classified as "other").
			return errors.New("something generic")
		case 3:
			cancel()
			return bctx.Err()
		}
		return nil
	}

	var buf bytes.Buffer
	Loop(ctx, testLogger(&buf), "test-metrics", 10*time.Millisecond, body,
		WithExitFn(func(int) {}),
		WithMetrics(metrics))

	// tick 1: network error (counts as tick + error)
	// tick 2: other error (counts as tick + error)
	// tick 3: ctx cancelled (counts as tick)
	if got := metrics.tickCount("test-metrics"); got < 3 {
		t.Fatalf("expected at least 3 ticks, got %d", got)
	}
	if got := metrics.errorCount("test-metrics", "network"); got != 1 {
		t.Fatalf("expected 1 'network' error, got %d", got)
	}
	if got := metrics.errorCount("test-metrics", "other"); got != 1 {
		t.Fatalf("expected 1 'other' error, got %d", got)
	}
}

// --- Loop log field tests ---

func TestLoop_LogFields(t *testing.T) {
	t.Parallel()

	t.Run("start", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // immediate cancel

		var buf bytes.Buffer
		Loop(ctx, testLogger(&buf), "log-test", 1*time.Second, func(ctx context.Context) error {
			return ctx.Err()
		}, WithExitFn(func(int) {}))

		output := buf.String()
		if !strings.Contains(output, `loop=log-test`) {
			t.Fatal("missing loop field in start log")
		}
		if !strings.Contains(output, "supervise loop started") {
			t.Fatal("missing start message")
		}
		if !strings.Contains(output, "interval=") {
			t.Fatal("missing interval field in start log")
		}
	})

	t.Run("stop", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		var buf bytes.Buffer
		Loop(ctx, testLogger(&buf), "log-stop", 1*time.Second, func(ctx context.Context) error {
			return ctx.Err()
		}, WithExitFn(func(int) {}))

		output := buf.String()
		if !strings.Contains(output, "supervise loop stopped") {
			t.Fatal("missing stop message")
		}
		if !strings.Contains(output, "ticks=") {
			t.Fatal("missing ticks field in stop log")
		}
		if !strings.Contains(output, "errors=") {
			t.Fatal("missing errors field in stop log")
		}
	})

	t.Run("tick_error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var calls atomic.Int32
		var buf bytes.Buffer
		Loop(ctx, testLogger(&buf), "log-err", 10*time.Millisecond, func(ctx context.Context) error {
			if calls.Add(1) == 1 {
				return errors.New("oops")
			}
			cancel()
			return ctx.Err()
		}, WithExitFn(func(int) {}))

		output := buf.String()
		if !strings.Contains(output, "supervise tick error") {
			t.Fatal("missing tick error message")
		}
		if !strings.Contains(output, "kind=") {
			t.Fatal("missing kind field in error log")
		}
		if !strings.Contains(output, "err=") {
			t.Fatal("missing err field in error log")
		}
	})

	t.Run("panic", func(t *testing.T) {
		var buf bytes.Buffer
		Loop(context.Background(), testLogger(&buf), "log-panic", 1*time.Second, func(_ context.Context) error {
			panic("boom")
		}, WithExitFn(func(int) {}))

		output := buf.String()
		if !strings.Contains(output, "supervise loop panic") {
			t.Fatal("missing panic message")
		}
		if !strings.Contains(output, "panic=") {
			t.Fatal("missing panic field")
		}
		if !strings.Contains(output, "stack=") {
			t.Fatal("missing stack field")
		}
	})

	t.Run("unexpected_return", func(t *testing.T) {
		var buf bytes.Buffer
		Loop(context.Background(), testLogger(&buf), "log-unexp", 1*time.Second, func(_ context.Context) error {
			return nil
		}, WithExitFn(func(int) {}))

		output := buf.String()
		if !strings.Contains(output, "supervise loop body returned unexpectedly") {
			t.Fatal("missing unexpected return message")
		}
	})
}

// --- Guard tests ---

func TestGuard_PanicIsFatal(t *testing.T) {
	t.Parallel()
	var exitCode atomic.Int32
	exitCode.Store(-1)
	var buf bytes.Buffer
	logger := testLogger(&buf)

	func() {
		defer GuardWithExitFn(context.Background(), logger, "test-guard-panic", func(code int) {
			exitCode.Store(int32(code))
		})()
		panic("guard panic test")
	}()

	if got := exitCode.Load(); got != 1 {
		t.Fatalf("expected exitFn(1) on panic, got %d", got)
	}
	if !strings.Contains(buf.String(), "guarded goroutine panic") {
		t.Fatal("expected panic log message")
	}
	if !strings.Contains(buf.String(), "guard panic test") {
		t.Fatal("expected panic value in log")
	}
	if !strings.Contains(buf.String(), `name=test-guard-panic`) {
		t.Fatal("expected name field in log")
	}
}

func TestGuard_EarlyReturnWithLiveCtxIsFatal(t *testing.T) {
	t.Parallel()
	var exitCode atomic.Int32
	exitCode.Store(-1)
	var buf bytes.Buffer
	logger := testLogger(&buf)

	func() {
		defer GuardWithExitFn(context.Background(), logger, "test-guard-early", func(code int) {
			exitCode.Store(int32(code))
		})()
		// Return without ctx being done — simulates unexpected early exit.
	}()

	if got := exitCode.Load(); got != 1 {
		t.Fatalf("expected exitFn(1) on early return, got %d", got)
	}
	if !strings.Contains(buf.String(), "guarded goroutine exited unexpectedly") {
		t.Fatal("expected unexpected exit log message")
	}
}

func TestGuard_CleanReturnAfterCtxCancel(t *testing.T) {
	t.Parallel()
	var exitCalled atomic.Int32
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Simulate shutdown.

	func() {
		defer GuardWithExitFn(ctx, logger, "test-guard-clean", func(_ int) {
			exitCalled.Add(1)
		})()
		// Return after ctx is done — clean exit.
	}()

	if exitCalled.Load() != 0 {
		t.Fatal("exitFn should not be called when ctx is already cancelled")
	}
}

func TestGuard_NormalBodyNoFatal(t *testing.T) {
	t.Parallel()
	var exitCalled atomic.Int32
	var buf bytes.Buffer
	logger := testLogger(&buf)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	func() {
		defer GuardWithExitFn(ctx, logger, "test-guard-normal", func(_ int) {
			exitCalled.Add(1)
		})()
		// Normal work, then return after ctx done.
		_ = 1 + 1
	}()

	if exitCalled.Load() != 0 {
		t.Fatal("exitFn should not be called for normal body with cancelled ctx")
	}
}

// --- ClassifyError tests ---

func TestClassifyError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil returns other",
			err:  nil,
			want: "other",
		},
		{
			name: "context.Canceled",
			err:  context.Canceled,
			want: "context",
		},
		{
			name: "context.DeadlineExceeded",
			err:  context.DeadlineExceeded,
			want: "context",
		},
		{
			name: "wrapped context.Canceled",
			err:  fmt.Errorf("operation failed: %w", context.Canceled),
			want: "context",
		},
		{
			name: "net.OpError",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: errors.New("connection refused"),
			},
			want: "network",
		},
		{
			name: "url.Error",
			err: &url.Error{
				Op:  "Get",
				URL: "https://example.com",
				Err: errors.New("connection refused"),
			},
			want: "network",
		},
		{
			name: "wrapped net.OpError",
			err: fmt.Errorf("registry call failed: %w", &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: errors.New("timeout"),
			}),
			want: "network",
		},
		{
			name: "context.DeadlineExceeded wrapped in net.OpError (network wins)",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: context.DeadlineExceeded,
			},
			want: "network",
		},
		{
			name: "context.DeadlineExceeded wrapped in url.Error (network wins)",
			err: &url.Error{
				Op:  "Get",
				URL: "https://example.com",
				Err: context.DeadlineExceeded,
			},
			want: "network",
		},
		{
			name: "auth 401",
			err:  errors.New("registry returned 401 unauthorized"),
			want: "auth",
		},
		{
			name: "auth 403",
			err:  errors.New("access denied: 403 Forbidden"),
			want: "auth",
		},
		{
			name: "auth unauthorized",
			err:  errors.New("authentication required"),
			want: "auth",
		},
		{
			name: "strconv.ErrSyntax via NumError",
			err:  strconv.ErrSyntax,
			want: "parse",
		},
		{
			name: "strconv parse error",
			err: &strconv.NumError{
				Func: "Atoi",
				Num:  "abc",
				Err:  strconv.ErrSyntax,
			},
			want: "parse",
		},
		{
			name: "JSON unmarshal error",
			err:  errors.New("cannot unmarshal object into string"),
			want: "parse",
		},
		{
			name: "unexpected end of JSON",
			err:  errors.New("unexpected end of JSON input"),
			want: "parse",
		},
		{
			name: "invalid character JSON",
			err:  errors.New("invalid character 'x' looking for beginning of value"),
			want: "parse",
		},
		{
			name: "generic error",
			err:  errors.New("something went wrong"),
			want: "other",
		},
		{
			name: "classified interface - parse",
			err:  &classifiableError{kind: "parse"},
			want: "parse",
		},
		{
			name: "classified interface - auth",
			err:  &classifiableError{kind: "auth"},
			want: "auth",
		},
		{
			name: "classified interface - unknown kind returns other",
			err:  &classifiableError{kind: "custom"},
			want: "other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyError(tt.err)
			if got != tt.want {
				t.Errorf("ClassifyError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// classifiableError implements the Classify() string interface.
type classifiableError struct {
	kind string
}

func (e *classifiableError) Error() string    { return "classifiable: " + e.kind }
func (e *classifiableError) Classify() string { return e.kind }

// Package supervise provides supervised loop primitives enforcing the
// cross-cutting invariants for all background goroutines in the operator:
//
//   - A dead ticker is a fatal bug. If the loop body panics or the loop exits
//     for any reason other than ctx.Done(), the process logs a stack trace and
//     exits non-zero.
//   - Per-tick errors are logged and retried. A single transient failure does
//     not kill the loop.
//   - ctx.Done() is the only clean exit.
//
// Loop runs a body on a ticker; Guard wraps an existing for/select loop with
// the same fatal-on-unexpected-exit semantics.
package supervise

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

// Metrics is the narrow interface for recording loop liveness and error
// counters. Implementations must be safe for concurrent use. A nil Metrics
// value is never passed to Loop; callers that omit WithMetrics get a no-op
// implementation.
type Metrics interface {
	IncTick(name string)
	IncError(name, kind string)
}

// Option configures Loop behavior.
type Option func(*loopConfig)

// WithMetrics injects a Metrics implementation into Loop. When nil or omitted,
// metric calls are no-ops.
func WithMetrics(m Metrics) Option {
	return func(c *loopConfig) {
		if m != nil {
			c.metrics = m
		}
	}
}

// WithExitFn overrides the function called on fatal conditions (default
// os.Exit). Used in tests to capture fatal invocations without terminating
// the test binary.
func WithExitFn(fn func(int)) Option {
	return func(c *loopConfig) {
		if fn != nil {
			c.exitFn = fn
		}
	}
}

// WithPerTickBody marks the body as a single-tick function that is EXPECTED
// to return nil after each successful tick (e.g. imageupdate.Poller.Tick or
// executor.RefreshImage). For such bodies the ticker itself provides the
// looping, so a nil return is a healthy completed tick rather than the
// "loop body returned unexpectedly" fatal condition that applies to bodies
// which own their own for/select loop. Errors and panics remain handled
// exactly as before (logged/warned and retried; panics fatal).
func WithPerTickBody() Option {
	return func(c *loopConfig) {
		c.perTickBody = true
	}
}

type loopConfig struct {
	metrics     Metrics
	exitFn      func(int)
	perTickBody bool // when true, a nil return from body is a completed tick, not a fatal bug
	halted      bool // set by executeTick on fatal; breaks the loop in tests where exitFn doesn't actually exit
}

type nopMetrics struct{}

func (nopMetrics) IncTick(string)          {}
func (nopMetrics) IncError(string, string) {}

func newLoopConfig(opts []Option) *loopConfig {
	cfg := &loopConfig{
		metrics: nopMetrics{},
		exitFn:  os.Exit,
	}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

// Loop runs body on a ticker until ctx is cancelled. Any panic in body, or any
// return from body with a nil error while ctx is still live, is fatal (stack
// trace via slog + exitFn(1)).
//
//   - name: used in log messages ("image-refresher").
//   - interval: 0 means "run body once and return" (no ticker, no fatal on
//     nil return after single execution).
//   - body: called once immediately, then every interval. Errors are logged at
//     warn and the loop continues; panics are fatal.
func Loop(ctx context.Context, logger *slog.Logger, name string, interval time.Duration, body func(context.Context) error, opts ...Option) {
	cfg := newLoopConfig(opts)
	l := logger.With(slog.String("loop", name))

	l.InfoContext(ctx, "supervise loop started", slog.Duration("interval", interval))

	var ticks, errs int

	// interval == 0: run body once and return (no fatal on clean return).
	if interval == 0 {
		executeTick(ctx, l, name, body, cfg, &ticks, &errs, false)
		l.InfoContext(ctx, "supervise loop stopped", slog.Int("ticks", ticks), slog.Int("errors", errs))
		return
	}

	// Normal ticker loop: immediate first tick, then every interval.
	executeTick(ctx, l, name, body, cfg, &ticks, &errs, true)
	if cfg.halted {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			l.InfoContext(ctx, "supervise loop stopped", slog.Int("ticks", ticks), slog.Int("errors", errs))
			return
		case <-ticker.C:
			executeTick(ctx, l, name, body, cfg, &ticks, &errs, true)
			if cfg.halted {
				return
			}
		}
	}
}

// executeTick runs body once with panic recovery. fatalOnNilReturn controls
// whether a nil return with a live ctx triggers exitFn (true for ticker loops,
// false for interval==0 single-execution mode).
func executeTick(ctx context.Context, l *slog.Logger, name string, body func(context.Context) error, cfg *loopConfig, ticks *int, errs *int, fatalOnNilReturn bool) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			// Trim stack to 4 KiB.
			if len(stack) > 4096 {
				stack = stack[:4096]
			}
			l.ErrorContext(ctx, "supervise loop panic",
				slog.Any("panic", r),
				slog.String("stack", string(stack)),
			)
			cfg.halted = true
			cfg.exitFn(1)
		}
	}()

	err := body(ctx)
	if err != nil {
		kind := ClassifyError(err)
		l.WarnContext(ctx, "supervise tick error",
			slog.String("kind", kind),
			slog.String("err", err.Error()),
		)
		cfg.metrics.IncError(name, kind)
		*errs++
		*ticks++
		cfg.metrics.IncTick(name)
		return
	}

	// body returned nil.
	if fatalOnNilReturn && !cfg.perTickBody && ctx.Err() == nil {
		// Unexpected: a self-looping body completed without error while ctx
		// is still live. Per-tick bodies (WithPerTickBody) legitimately
		// return nil after each successful tick and fall through to normal
		// completion below.
		l.ErrorContext(ctx, "supervise loop body returned unexpectedly")
		cfg.halted = true
		cfg.exitFn(1)
		return
	}

	// Normal completion.
	*ticks++
	cfg.metrics.IncTick(name)
}

// Guard is the defer-idiom used by callers that manage their own for/select
// loop. It fatals if the surrounding function returns while ctx is still live,
// or if a panic is unwinding.
//
// Usage:
//
//	defer supervise.Guard(ctx, logger, "dispatchLoop")()
func Guard(ctx context.Context, logger *slog.Logger, name string) func() {
	return guardWithExit(ctx, logger, name, os.Exit)
}

// GuardWithExitFn is like Guard but with a custom exit function for testing.
func GuardWithExitFn(ctx context.Context, logger *slog.Logger, name string, exitFn func(int)) func() {
	return guardWithExit(ctx, logger, name, exitFn)
}

func guardWithExit(ctx context.Context, logger *slog.Logger, name string, exitFn func(int)) func() {
	return func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			if len(stack) > 4096 {
				stack = stack[:4096]
			}
			logger.Error("guarded goroutine panic",
				slog.String("name", name),
				slog.Any("panic", r),
				slog.String("stack", string(stack)),
			)
			exitFn(1)
			return
		}

		// No panic — check if ctx is still live (unexpected exit).
		if ctx.Err() == nil {
			logger.Error("guarded goroutine exited unexpectedly",
				slog.String("name", name),
			)
			exitFn(1)
		}
	}
}

// ClassifyError returns a low-cardinality error kind suitable for metric labels.
// The five possible values are: "network", "context", "parse", "auth", "other".
func ClassifyError(err error) string {
	if err == nil {
		return "other"
	}

	// Network errors — check first because context errors can be wrapped in net errors.
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return "network"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return "network"
	}

	// Context errors.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "context"
	}

	// Check for Classified interface — lets callers define custom classification.
	var classified interface {
		Classify() string
	}
	if errors.As(err, &classified) {
		kind := classified.Classify()
		switch kind {
		case "network", "context", "parse", "auth":
			return kind
		default:
			return "other"
		}
	}

	// Auth errors — check for common HTTP status codes in error messages.
	msg := err.Error()
	if containsAuthIndicator(msg) {
		return "auth"
	}

	// Parse errors — check for common parse-related patterns.
	if isParseError(msg) {
		return "parse"
	}

	return "other"
}

// containsAuthIndicator checks if an error message indicates an authentication
// or authorization failure.
func containsAuthIndicator(msg string) bool {
	lower := strings.ToLower(msg)
	indicators := []string{
		"401",
		"403",
		"unauthorized",
		"forbidden",
		"authentication required",
		"access denied",
	}
	for _, ind := range indicators {
		if strings.Contains(lower, ind) {
			return true
		}
	}
	return false
}

// isParseError checks if the error message indicates a parse failure.
func isParseError(msg string) bool {
	parseIndicators := []string{
		"invalid syntax",
		"parsing",
		"unexpected end of JSON",
		"cannot unmarshal",
		"invalid character",
	}
	for _, ind := range parseIndicators {
		if strings.Contains(msg, ind) {
			return true
		}
	}
	return false
}

// Ensure nopMetrics satisfies the Metrics interface at compile time.
var _ Metrics = nopMetrics{}

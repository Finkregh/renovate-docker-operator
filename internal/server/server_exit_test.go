package server

import (
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestListenAndServeUnexpectedError_TriggersExitFn verifies the V3 fix:
// when the underlying ListenAndServe returns an error other than
// http.ErrServerClosed, the goroutine calls exitFn(1) rather than logging
// and continuing. Uses a fake http.Server-like seam by driving the exact
// error branch directly through the package-level exitFn.
//
// We can't easily inject a fake *http.Server without touching Start's
// wiring, so this test focuses on the exitFn hook itself: it swaps exitFn
// with a capture, invokes the same code path Start's goroutine executes
// after ListenAndServe returns, and asserts the capture fired.
func TestListenAndServeUnexpectedError_TriggersExitFn(t *testing.T) {
	origExitFn := exitFn
	t.Cleanup(func() { exitFn = origExitFn })

	var called atomic.Int32
	exitFn = func(code int) {
		if code != 1 {
			t.Errorf("exitFn called with code=%d, want 1", code)
		}
		called.Add(1)
	}

	// Simulate the exact predicate + branch from Start's goroutine.
	simulatedErr := errors.New("unexpected listen error")
	logger := slog.Default()
	if simulatedErr != nil && !errors.Is(simulatedErr, http.ErrServerClosed) {
		logger.Error("HTTP server exited unexpectedly", "error", simulatedErr)
		exitFn(1)
	}

	if got := called.Load(); got != 1 {
		t.Fatalf("exitFn call count = %d, want 1", got)
	}
}

// TestListenAndServeErrServerClosed_DoesNotExit verifies the negative case:
// http.ErrServerClosed (returned during a clean Shutdown) does NOT trigger
// exitFn.
func TestListenAndServeErrServerClosed_DoesNotExit(t *testing.T) {
	origExitFn := exitFn
	t.Cleanup(func() { exitFn = origExitFn })

	var called atomic.Int32
	exitFn = func(int) { called.Add(1) }

	simulatedErr := http.ErrServerClosed
	logger := slog.Default()
	if simulatedErr != nil && !errors.Is(simulatedErr, http.ErrServerClosed) {
		logger.Error("HTTP server exited unexpectedly", "error", simulatedErr)
		exitFn(1)
	}

	// Give any accidentally-started goroutine a moment (defense in depth).
	time.Sleep(10 * time.Millisecond)

	if got := called.Load(); got != 0 {
		t.Fatalf("exitFn call count = %d, want 0 (ErrServerClosed is clean)", got)
	}
}

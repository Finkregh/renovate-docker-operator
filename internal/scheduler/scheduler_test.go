package scheduler

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.Default()
}

func TestNewScheduler(t *testing.T) {
	s := New(testLogger())
	if s == nil {
		t.Fatal("New() returned nil")
	}
	if s.cron == nil {
		t.Fatal("New() did not initialize cron")
	}
	if s.logger == nil {
		t.Fatal("New() did not initialize logger")
	}
}

func TestAddFunc_ValidSchedule(t *testing.T) {
	s := New(testLogger())

	err := s.AddFunc("@every 1s", "test-job", func() {})
	if err != nil {
		t.Fatalf("AddFunc() with valid schedule returned error: %v", err)
	}
}

func TestAddFunc_InvalidSchedule(t *testing.T) {
	s := New(testLogger())

	err := s.AddFunc("not-a-valid-cron", "bad-job", func() {})
	if err == nil {
		t.Fatal("AddFunc() with invalid schedule should return error, got nil")
	}
}

func TestStartAndStop(t *testing.T) {
	s := New(testLogger())

	var count atomic.Int64

	err := s.AddFunc("@every 1s", "counter-job", func() {
		count.Add(1)
	})
	if err != nil {
		t.Fatalf("AddFunc() failed: %v", err)
	}

	s.Start()

	// Wait up to 2.5 seconds for at least one execution
	deadline := time.After(2500 * time.Millisecond)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	fired := false
	for !fired {
		select {
		case <-deadline:
			t.Fatalf("job did not fire within 2.5s; count=%d", count.Load())
		case <-ticker.C:
			if count.Load() > 0 {
				fired = true
			}
		}
	}

	s.Stop()

	// After stop, no more increments should happen
	countAtStop := count.Load()
	time.Sleep(1500 * time.Millisecond)
	countAfterWait := count.Load()

	if countAfterWait > countAtStop+1 {
		// Allow at most 1 extra tick that was already in-flight
		t.Fatalf("job continued firing after Stop(); countAtStop=%d countAfterWait=%d",
			countAtStop, countAfterWait)
	}
}

func TestGetNextRunOnSchedule_Valid(t *testing.T) {
	s := New(testLogger())

	// Standard 5-field cron: every minute
	next := s.GetNextRunOnSchedule("* * * * *")
	if next.IsZero() {
		t.Fatal("GetNextRunOnSchedule() returned zero time for valid expression")
	}

	// Next run should be within 60 seconds from now
	until := time.Until(next)
	if until <= 0 || until > 61*time.Second {
		t.Fatalf("GetNextRunOnSchedule() returned unexpected time: %v (until=%v)", next, until)
	}
}

func TestGetNextRunOnSchedule_Invalid(t *testing.T) {
	s := New(testLogger())

	next := s.GetNextRunOnSchedule("invalid expression")
	if !next.IsZero() {
		t.Fatalf("GetNextRunOnSchedule() should return zero time for invalid expression, got %v", next)
	}
}

func TestStopWithoutStart(t *testing.T) {
	t.Parallel()
	s := New(testLogger())

	// Stop without Start should not panic
	s.Stop()
}

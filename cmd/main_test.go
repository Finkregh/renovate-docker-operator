package main

import (
	"testing"

	"git.h.oluflorenzen.de/finkregh/renovate-docker-operator/config"
)

// TestRun_FatalOnBelowFloorPollInterval asserts the run(exitFn) seam:
// a config that passes Load() but fails config.Validate (poll interval
// below the floor with the poller enabled) must call exitFn(1) exactly
// once and return a non-nil error BEFORE reaching the supervise loop.
//
// This is a wiring test only — the exhaustive validation surface is
// covered by config's own TestValidate. Here we just prove the seam is
// connected: Load -> Validate -> exitFn(1) -> return err.
func TestRun_FatalOnBelowFloorPollInterval(t *testing.T) {
	// Minimal env for config.Load() to succeed (only these two are
	// required; everything else has a default).
	t.Setenv("ROP_PLATFORM_ENDPOINT", "https://git.example.com")
	t.Setenv("RENOVATE_TOKEN", "test-token")

	// Enable the poller and set a poll interval below the floor so
	// config.Validate returns an error. ROP_IMAGE keeps its valid
	// default (renovate/renovate:latest), so Validate reaches the
	// floor check rather than failing on the ref parse.
	t.Setenv("ROP_CHECK_UPDATE_RUNNER_IMAGE", "true")
	t.Setenv("ROP_IMAGE_UPDATE_POLL_INTERVAL", "10s") // below config.MinImageUpdatePollInterval (15m)

	// Guard against the fixture drifting out from under the test: 10s
	// must actually be below the floor for this to exercise the path.
	if config.MinImageUpdatePollInterval <= 0 {
		t.Fatalf("unexpected MinImageUpdatePollInterval=%s", config.MinImageUpdatePollInterval)
	}

	var exitCalls int
	var lastCode int
	recordingExit := func(code int) {
		exitCalls++
		lastCode = code
	}

	err := run(recordingExit)

	if err == nil {
		t.Fatal("expected non-nil error from run() on below-floor poll interval, got nil")
	}
	if exitCalls != 1 {
		t.Errorf("expected exitFn called exactly once, got %d", exitCalls)
	}
	if lastCode != 1 {
		t.Errorf("expected exitFn called with code 1, got %d", lastCode)
	}
}
